import Foundation

// MARK: - Engine contract: `pgbot sql run --json` (sql_run_schema_version 1.x)

public struct SQLRunColumn: Codable, Sendable, Equatable, Hashable {
    public var name: String
    public var type: String
    public init(name: String, type: String) { self.name = name; self.type = type }

    /// Right-align numbers, like psql.
    public var isNumeric: Bool {
        let t = type.lowercased()
        return ["integer", "bigint", "smallint", "real", "double precision", "money", "oid"].contains(t) || t.hasPrefix("numeric")
    }
}

public struct SQLRunResult: Codable, Sendable, Equatable {
    public var statement: String
    public var command: String
    public var columns: [SQLRunColumn]
    public var rows: [[String?]]
    public var rowCount: Int
    public var truncated: Bool
    public var elapsedMS: Double
    enum CodingKeys: String, CodingKey {
        case statement, command, columns, rows, rowCount = "row_count", truncated, elapsedMS = "elapsed_ms"
    }
    public init(statement: String, command: String, columns: [SQLRunColumn], rows: [[String?]], rowCount: Int, truncated: Bool, elapsedMS: Double) {
        self.statement = statement; self.command = command; self.columns = columns; self.rows = rows
        self.rowCount = rowCount; self.truncated = truncated; self.elapsedMS = elapsedMS
    }
}

public struct SQLRunError: Codable, Sendable, Equatable {
    public var statementIndex: Int
    public var statement: String?
    public var message: String
    public var sqlstate: String?
    public var detail: String?
    public var hint: String?
    public var position: Int?
    enum CodingKeys: String, CodingKey { case statementIndex = "statement_index", statement, message, sqlstate, detail, hint, position }
    public init(statementIndex: Int = -1, statement: String? = nil, message: String, sqlstate: String? = nil,
                detail: String? = nil, hint: String? = nil, position: Int? = nil) {
        self.statementIndex = statementIndex; self.statement = statement; self.message = message
        self.sqlstate = sqlstate; self.detail = detail; self.hint = hint; self.position = position
    }
    /// True when pgbot (not the server) refused the statement.
    public var isRefusal: Bool { sqlstate == "PGBOT" }
}

public struct SQLRunOutput: Codable, Sendable, Equatable {
    public var readOnly: Bool
    public var results: [SQLRunResult]
    public var error: SQLRunError?
    public var elapsedMS: Double
    enum CodingKeys: String, CodingKey { case readOnly = "read_only", results, error, elapsedMS = "elapsed_ms" }
    public init(readOnly: Bool = true, results: [SQLRunResult], error: SQLRunError?, elapsedMS: Double) {
        self.readOnly = readOnly; self.results = results; self.error = error; self.elapsedMS = elapsedMS
    }
}

/// A psql exchange as stored in the conversation.
public struct PSQLResultPayload: Codable, Sendable, Equatable {
    public var input: String
    /// e.g. `List of relations` for meta-commands.
    public var title: String?
    public var output: SQLRunOutput
    public var expanded: Bool
    public init(input: String, title: String?, output: SQLRunOutput, expanded: Bool) {
        self.input = input; self.title = title; self.output = output; self.expanded = expanded
    }

    /// Tab-separated text for copy, search and export.
    public var plainText: String {
        var parts = [input]
        for r in output.results {
            if r.columns.isEmpty { parts.append(r.command); continue }
            parts.append(r.columns.map(\.name).joined(separator: "\t"))
            parts += r.rows.map { $0.map { $0 ?? "" }.joined(separator: "\t") }
        }
        if let e = output.error { parts.append("ERROR: \(e.message)") }
        return parts.joined(separator: "\n")
    }
}

// MARK: - psql input handling

public enum PSQLCommand: Equatable, Sendable {
    /// Plain SQL, sent as typed.
    case sql(String)
    /// A meta-command translated to catalog SQL (run read-only like any other).
    case meta(sql: String, title: String)
    case toggleExpanded(Bool?)
    case help
    case quit
    case unsupported(String, reason: String)
}

public enum PSQL {
    /// Parses one line of console input the way psql would.
    public static func parse(_ raw: String) -> PSQLCommand {
        let input = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard input.hasPrefix("\\") else { return .sql(input) }
        let parts = input.dropFirst().split(separator: " ", maxSplits: 1).map(String.init)
        let cmd = parts.first ?? ""
        let arg = parts.count > 1 ? parts[1].trimmingCharacters(in: .whitespaces).trimmingCharacters(in: CharacterSet(charactersIn: ";")) : ""
        let plus = cmd.hasSuffix("+")
        let base = plus ? String(cmd.dropLast()) : cmd

        switch base {
        case "?", "h", "help": return .help
        case "q", "quit": return .quit
        case "x":
            switch arg.lowercased() { case "on": return .toggleExpanded(true); case "off": return .toggleExpanded(false); default: return .toggleExpanded(nil) }
        case "l", "list": return .meta(sql: listDatabases, title: "List of databases")
        case "dn": return .meta(sql: listSchemas(pattern: arg), title: "List of schemas")
        case "d" where !arg.isEmpty: return describe(arg, verbose: plus)
        case "d": return .meta(sql: listRelations(kinds: ["r", "p", "v", "m", "S", "f"], pattern: arg, verbose: plus), title: "List of relations")
        case "dt": return .meta(sql: listRelations(kinds: ["r", "p"], pattern: arg, verbose: plus), title: "List of tables")
        case "dv": return .meta(sql: listRelations(kinds: ["v"], pattern: arg, verbose: plus), title: "List of views")
        case "dm": return .meta(sql: listRelations(kinds: ["m"], pattern: arg, verbose: plus), title: "List of materialized views")
        case "ds": return .meta(sql: listRelations(kinds: ["S"], pattern: arg, verbose: plus), title: "List of sequences")
        case "di": return .meta(sql: listRelations(kinds: ["i", "I"], pattern: arg, verbose: plus), title: "List of indexes")
        case "df": return .meta(sql: listFunctions(pattern: arg), title: "List of functions")
        case "du", "dg": return .meta(sql: listRoles, title: "List of roles")
        case "dx": return .meta(sql: listExtensions, title: "List of installed extensions")
        case "conninfo": return .meta(sql: connInfo, title: "Connection")
        case "c", "connect":
            return .unsupported(input, reason: "Each database bot is one connection. Pick another database in the sidebar to switch.")
        case "!", "o", "out", "i", "ir", "include", "w", "write", "copy", "g", "gexec", "gset", "e", "edit", "ef", "ev", "set", "unset", "password", "lo_import", "lo_export", "setenv", "cd":
            return .unsupported(input, reason: "\\\(base) touches files, the shell or session state, which PGBot's read-only console doesn't allow.")
        default:
            return .unsupported(input, reason: "PGBot doesn't support \\\(base) yet. Type \\? for the commands it does.")
        }
    }

    /// psql semantics: a meta-command runs on Return; SQL runs once it ends
    /// with a semicolon outside quotes, dollar-quotes and comments.
    public static func isComplete(_ raw: String) -> Bool {
        let s = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        if s.isEmpty { return false }
        if s.hasPrefix("\\") { return true }
        var last: Character? = nil
        var i = s.startIndex
        var quote: Character? = nil
        var dollarTag: String? = nil
        while i < s.endIndex {
            let c = s[i]
            if let tag = dollarTag {
                if s[i...].hasPrefix(tag) { i = s.index(i, offsetBy: tag.count); dollarTag = nil; last = "$"; continue }
                i = s.index(after: i); continue
            }
            if let q = quote {
                if c == q { quote = nil; last = c }
                i = s.index(after: i); continue
            }
            if c == "-", s[i...].hasPrefix("--") {
                while i < s.endIndex && s[i] != "\n" { i = s.index(after: i) }
                continue
            }
            if c == "/", s[i...].hasPrefix("/*") {
                if let end = s[i...].range(of: "*/") { i = end.upperBound } else { return false }
                continue
            }
            if c == "'" || c == "\"" { quote = c; i = s.index(after: i); continue }
            if c == "$", let m = s[i...].firstMatch(of: #/^\$[A-Za-z_]*\$/#) {
                dollarTag = String(m.output); i = s.index(i, offsetBy: m.output.count); continue
            }
            if !c.isWhitespace { last = c }
            i = s.index(after: i)
        }
        return quote == nil && dollarTag == nil && last == ";"
    }

    public static let helpText = """
    **PSQL mode** runs your SQL in a read-only transaction that's always rolled back. Queries and catalog lookups work; changes don't.

    End a statement with `;` and press Return to run it. Shift-Return adds a line, ↑ and ↓ walk your history.

    | Command | Shows |
    |---|---|
    | `\\d name` | a table's columns and indexes |
    | `\\d`, `\\dt`, `\\dv`, `\\dm`, `\\di`, `\\ds` | relations, tables, views, materialized views, indexes, sequences (add `+` for sizes) |
    | `\\dn`, `\\df`, `\\dx` | schemas, functions, extensions |
    | `\\l`, `\\du` | databases, roles |
    | `\\conninfo` | who and where you're connected |
    | `\\x` | expanded (one row per record) display |
    | `\\q` | back to chat |

    Patterns work like psql: `\\dt public.order*`.
    """

    // MARK: Pattern → SQL (psql's rules, safely quoted)

    /// Converts a psql pattern to (schema regex?, name regex). `*` → `.*`,
    /// `?` → `.`, unquoted text folds to lower case, double quotes keep case.
    static func regexParts(_ pattern: String) -> (schema: String?, name: String)? {
        guard !pattern.isEmpty else { return nil }
        var parts: [String] = [""]
        var inQuotes = false
        for c in pattern {
            switch c {
            case "\"": inQuotes.toggle()
            case "." where !inQuotes: parts.append("")
            case "*" where !inQuotes: parts[parts.count - 1] += ".*"
            case "?" where !inQuotes: parts[parts.count - 1] += "."
            default:
                let ch = inQuotes ? String(c) : String(c).lowercased()
                parts[parts.count - 1] += "\\^$.|+()[]{}".contains(c) ? "\\" + ch : ch
            }
        }
        let rx = parts.map { "^(" + $0 + ")$" }
        return rx.count >= 2 ? (rx[rx.count - 2], rx[rx.count - 1]) : (nil, rx[0])
    }

    /// A single-quoted SQL literal with embedded quotes doubled.
    static func literal(_ s: String) -> String { "'" + s.replacingOccurrences(of: "'", with: "''") + "'" }

    static func patternCondition(_ pattern: String, schema: String, name: String, visible: String) -> String {
        guard let p = regexParts(pattern) else {
            return "\(schema) NOT IN ('pg_catalog', 'information_schema') AND \(schema) !~ '^pg_toast'"
        }
        if let s = p.schema { return "\(schema) ~ \(literal(s)) AND \(name) ~ \(literal(p.name))" }
        return "\(name) ~ \(literal(p.name)) AND \(visible)"
    }

    static let relkindLabel = """
    CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'partitioned table' WHEN 'v' THEN 'view' \
    WHEN 'm' THEN 'materialized view' WHEN 'i' THEN 'index' WHEN 'I' THEN 'partitioned index' \
    WHEN 'S' THEN 'sequence' WHEN 'f' THEN 'foreign table' END
    """

    static func listRelations(kinds: [String], pattern: String, verbose: Bool) -> String {
        let inList = kinds.map { "'\($0)'" }.joined(separator: ", ")
        let indexTable = kinds.contains("i") ? ", (SELECT t.relname FROM pg_index x JOIN pg_class t ON t.oid = x.indrelid WHERE x.indexrelid = c.oid) AS \"Table\"" : ""
        let extra = verbose ? ", pg_size_pretty(pg_total_relation_size(c.oid)) AS \"Size\", obj_description(c.oid, 'pg_class') AS \"Description\"" : ""
        return """
        SELECT n.nspname AS "Schema", c.relname AS "Name", \(relkindLabel) AS "Type", pg_get_userbyid(c.relowner) AS "Owner"\(indexTable)\(extra)
        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE c.relkind IN (\(inList)) AND \(patternCondition(pattern, schema: "n.nspname", name: "c.relname", visible: "pg_table_is_visible(c.oid)"))
        ORDER BY 1, 2;
        """
    }

    static func describe(_ pattern: String, verbose: Bool) -> PSQLCommand {
        guard let p = regexParts(pattern) else { return .unsupported("\\d", reason: "Give a table name, e.g. \\d orders") }
        let match = p.schema.map { "n.nspname ~ \(literal($0)) AND c.relname ~ \(literal(p.name))" }
            ?? "c.relname ~ \(literal(p.name))"
        let target = """
        (SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace \
        WHERE \(match) ORDER BY pg_table_is_visible(c.oid) DESC, n.nspname LIMIT 1)
        """
        let columns = """
        SELECT a.attname AS "Column", format_type(a.atttypid, a.atttypmod) AS "Type",
               CASE WHEN a.attnotnull THEN 'not null' ELSE '' END AS "Nullable",
               coalesce(pg_get_expr(d.adbin, d.adrelid), '') AS "Default"
        FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
        WHERE a.attrelid = \(target) AND a.attnum > 0 AND NOT a.attisdropped
        ORDER BY a.attnum;
        """
        let indexes = """
        SELECT i.relname AS "Index", pg_get_indexdef(i.oid) AS "Definition"\(verbose ? ", pg_size_pretty(pg_relation_size(i.oid)) AS \"Size\"" : "")
        FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid
        WHERE x.indrelid = \(target)
        ORDER BY 1;
        """
        return .meta(sql: columns + "\n" + indexes, title: "Table \(pattern)")
    }

    static func listSchemas(pattern: String) -> String {
        let cond = regexParts(pattern).map { "n.nspname ~ \(literal($0.name))" } ?? "n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'"
        return "SELECT n.nspname AS \"Name\", pg_get_userbyid(n.nspowner) AS \"Owner\" FROM pg_namespace n WHERE \(cond) ORDER BY 1;"
    }

    static func listFunctions(pattern: String) -> String {
        """
        SELECT n.nspname AS "Schema", p.proname AS "Name", pg_get_function_result(p.oid) AS "Result data type",
               pg_get_function_arguments(p.oid) AS "Argument data types",
               CASE p.prokind WHEN 'a' THEN 'agg' WHEN 'w' THEN 'window' WHEN 'p' THEN 'proc' ELSE 'func' END AS "Type"
        FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE \(patternCondition(pattern, schema: "n.nspname", name: "p.proname", visible: "pg_function_is_visible(p.oid)"))
        ORDER BY 1, 2, 4;
        """
    }

    static let listDatabases = """
    SELECT d.datname AS "Name", pg_get_userbyid(d.datdba) AS "Owner", pg_encoding_to_char(d.encoding) AS "Encoding",
           CASE WHEN has_database_privilege(d.datname, 'CONNECT') THEN pg_size_pretty(pg_database_size(d.datname)) ELSE 'No Access' END AS "Size"
    FROM pg_database d WHERE NOT d.datistemplate ORDER BY 1;
    """

    static let listRoles = """
    SELECT r.rolname AS "Role name",
           concat_ws(', ', CASE WHEN r.rolsuper THEN 'Superuser' END, CASE WHEN r.rolcreaterole THEN 'Create role' END,
                     CASE WHEN r.rolcreatedb THEN 'Create DB' END, CASE WHEN NOT r.rolcanlogin THEN 'Cannot login' END,
                     CASE WHEN r.rolreplication THEN 'Replication' END, CASE WHEN r.rolbypassrls THEN 'Bypass RLS' END) AS "Attributes",
           ARRAY(SELECT b.rolname FROM pg_auth_members m JOIN pg_roles b ON m.roleid = b.oid WHERE m.member = r.oid ORDER BY 1)::text AS "Member of"
    FROM pg_roles r WHERE r.rolname !~ '^pg_' ORDER BY 1;
    """

    static let listExtensions = """
    SELECT e.extname AS "Name", e.extversion AS "Version", n.nspname AS "Schema", obj_description(e.oid, 'pg_extension') AS "Description"
    FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace ORDER BY 1;
    """

    static let connInfo = """
    SELECT current_database() AS "Database", current_user AS "User",
           coalesce(host(inet_server_addr()), 'local socket') AS "Server", coalesce(inet_server_port()::text, '') AS "Port",
           current_setting('server_version') AS "Version", current_setting('transaction_read_only') AS "Read only";
    """
}
