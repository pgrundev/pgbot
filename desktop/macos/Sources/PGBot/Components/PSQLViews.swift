import PGBotKit
import SwiftUI

private let mono = { (size: CGFloat, weight: Font.Weight) in Font.sys(size: size, weight: weight, design: .monospaced) }

/// What the user typed in PSQL mode, shown like a terminal line.
@MainActor struct PSQLInputBubble: View {
    let input: String
    let database: String
    var body: some View {
        let lines = input.components(separatedBy: "\n")
        VStack(alignment: .leading, spacing: 2) {
            ForEach(Array(lines.enumerated()), id: \.offset) { i, line in
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Text(i == 0 ? "\(database)=>" : String(repeating: " ", count: database.count) + "->")
                        .foregroundStyle(Palette.healthy)
                    Text(line).foregroundStyle(Palette.userText)
                }
            }
        }
        .font(mono(13.5, .regular))
        .textSelection(.enabled)
        .padding(.horizontal, 14).padding(.vertical, 10)
        .background(Palette.userBubble, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
    }
}

/// Results of one PSQL exchange: tables (or expanded records), command tags,
/// and psql-style errors.
@MainActor struct PSQLResultView: View {
    let payload: PSQLResultPayload

    var body: some View {
        let out = payload.output
        VStack(alignment: .leading, spacing: 12) {
            if let t = payload.title { Text(t).font(Typo.bodyEmphasis) }
            if let missing = missingRelation {
                Text("Did not find any relation named “\(missing)”.").font(mono(13, .regular)).foregroundStyle(.secondary)
            } else {
                ForEach(Array(out.results.enumerated()), id: \.offset) { i, r in
                    if r.columns.isEmpty {
                        Text(r.command.isEmpty ? "OK" : r.command).font(mono(13, .regular)).foregroundStyle(.secondary)
                    } else {
                        VStack(alignment: .leading, spacing: 6) {
                            if out.results.count > 1 && payload.title != nil, let sub = subtitle(i) {
                                Text(sub).font(Typo.small).foregroundStyle(.secondary)
                            }
                            if payload.expanded { PSQLRecords(result: r) } else { PSQLTable(result: r) }
                            PSQLFooter(result: r)
                        }
                    }
                }
            }
            if let e = out.error { PSQLErrorView(error: e) }
            if out.results.isEmpty && out.error == nil { Text("(no output)").font(mono(13, .regular)).foregroundStyle(.secondary) }
            HStack(spacing: 6) {
                Image(systemName: "lock.fill").font(.system(size: 9))
                Text("read-only · rolled back" + (out.elapsedMS > 0 ? " · \(Self.ms(out.elapsedMS))" : ""))
            }
            .font(Typo.micro).foregroundStyle(.tertiary)
        }
    }

    /// `\d name` that matched nothing returns empty column and index lists.
    private var missingRelation: String? {
        guard let t = payload.title, t.hasPrefix("Table "), payload.output.error == nil,
              payload.output.results.first?.rows.isEmpty ?? false else { return nil }
        return String(t.dropFirst(6))
    }

    private func subtitle(_ i: Int) -> String? {
        guard payload.title?.hasPrefix("Table ") == true else { return nil }
        return i == 0 ? "Columns" : "Indexes"
    }

    static func ms(_ v: Double) -> String { v < 1 ? String(format: "%.2f ms", v) : (v < 1000 ? String(format: "%.0f ms", v) : String(format: "%.2f s", v / 1000)) }
}

@MainActor struct PSQLTable: View {
    let result: SQLRunResult
    @State private var showAll = false
    private let preview = 100

    var body: some View {
        let rows = showAll ? result.rows : Array(result.rows.prefix(preview))
        VStack(alignment: .leading, spacing: 6) {
            ScrollView(.horizontal, showsIndicators: true) {
                Grid(alignment: .leading, horizontalSpacing: 0, verticalSpacing: 0) {
                    GridRow {
                        ForEach(Array(result.columns.enumerated()), id: \.offset) { _, c in
                            VStack(alignment: c.isNumeric ? .trailing : .leading, spacing: 1) {
                                Text(c.name).font(mono(12.5, .semibold))
                                Text(c.type).font(.sys(size: 10)).foregroundStyle(.tertiary)
                            }
                            .padding(.horizontal, 12).padding(.top, 8).padding(.bottom, 7)
                            .gridColumnAlignment(c.isNumeric ? .trailing : .leading)
                        }
                    }
                    // Views outside a GridRow span every column: full-width rules, psql-like.
                    Rectangle().fill(Palette.strongHairline).frame(height: 1)
                    ForEach(Array(rows.enumerated()), id: \.offset) { ri, row in
                        GridRow {
                            ForEach(Array(result.columns.enumerated()), id: \.offset) { ci, c in
                                PSQLCell(value: ci < row.count ? row[ci] : nil, numeric: c.isNumeric)
                            }
                        }
                        if ri < rows.count - 1 { Rectangle().fill(Palette.hairline).frame(height: 1) }
                    }
                }
                .fixedSize(horizontal: true, vertical: false)
            }
            .background(Palette.inset, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
            .clipShape(RoundedRectangle(cornerRadius: 12, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: 12, style: .continuous).strokeBorder(Palette.hairline))
            if result.rows.count > preview {
                Button(showAll ? "Show first \(preview) rows" : "Show all \(result.rows.count) rows") { withAnimation(.snappy) { showAll.toggle() } }
                    .buttonStyle(.pillSmall)
            }
        }
    }
}

@MainActor struct PSQLCell: View {
    let value: String?
    let numeric: Bool
    var body: some View {
        Group {
            if let v = value {
                Text(v.count > 160 ? String(v.prefix(159)) + "…" : v.replacingOccurrences(of: "\n", with: "↵"))
                    .help(v.count > 40 ? v : "")
            } else {
                Text("null").italic().foregroundStyle(.tertiary)
            }
        }
        .font(mono(12.5, .regular))
        .lineLimit(1)
        .textSelection(.enabled)
        .frame(maxWidth: 420, alignment: numeric ? .trailing : .leading)
        .padding(.horizontal, 12).padding(.vertical, 6)
    }
}

/// `\x` expanded display: one block per record.
@MainActor struct PSQLRecords: View {
    let result: SQLRunResult
    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            ForEach(Array(result.rows.prefix(50).enumerated()), id: \.offset) { i, row in
                VStack(alignment: .leading, spacing: 4) {
                    Text("─[ RECORD \(i + 1) ]─").font(mono(11.5, .semibold)).foregroundStyle(.secondary)
                    Grid(alignment: .leading, horizontalSpacing: 16, verticalSpacing: 3) {
                        ForEach(Array(result.columns.enumerated()), id: \.offset) { ci, c in
                            GridRow {
                                Text(c.name).font(mono(12.5, .semibold)).foregroundStyle(.secondary)
                                if let v = ci < row.count ? row[ci] : nil {
                                    Text(v).font(mono(12.5, .regular)).textSelection(.enabled)
                                } else {
                                    Text("null").font(mono(12.5, .regular)).italic().foregroundStyle(.tertiary)
                                }
                            }
                        }
                    }
                }
                .padding(12)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(Palette.inset, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
            }
            if result.rows.count > 50 { Text("Showing 50 of \(result.rows.count) records.").font(Typo.small).foregroundStyle(.secondary) }
        }
    }
}

@MainActor struct PSQLFooter: View {
    let result: SQLRunResult
    var body: some View {
        HStack(spacing: 8) {
            Text(rowsLabel).font(mono(12, .regular)).foregroundStyle(.secondary)
            Text(PSQLResultView.ms(result.elapsedMS)).font(Typo.micro).foregroundStyle(.tertiary)
            Spacer()
            Menu {
                Button("Copy as TSV") { Pasteboard.copy(PSQLExport.tsv(result)) }
                Button("Copy as CSV") { Pasteboard.copy(PSQLExport.csv(result)) }
                Button("Copy as Markdown") { Pasteboard.copy(PSQLExport.markdown(result)) }
            } label: {
                Label("Copy", systemImage: "doc.on.doc").font(Typo.small)
            }
            .menuStyle(.button).buttonStyle(.plain).menuIndicator(.hidden).fixedSize()
            .foregroundStyle(.secondary)
        }
    }

    private var rowsLabel: String {
        let n = result.rowCount
        return result.truncated ? "(first \(n) rows — more were not fetched)" : "(\(n) row\(n == 1 ? "" : "s"))"
    }
}

@MainActor struct PSQLErrorView: View {
    let error: SQLRunError
    var body: some View {
        VStack(alignment: .leading, spacing: 5) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text(error.isRefusal ? "BLOCKED" : "ERROR").font(mono(12.5, .bold)).foregroundStyle(error.isRefusal ? .orange : .red)
                Text(error.message).font(mono(12.5, .regular)).textSelection(.enabled)
            }
            if let caret = caretLines {
                VStack(alignment: .leading, spacing: 0) {
                    Text(caret.0)
                    Text(caret.1).foregroundStyle(.red)
                }
                .font(mono(12.5, .regular))
            }
            if let d = error.detail, !d.isEmpty { line("DETAIL", d) }
            if let h = error.hint, !h.isEmpty { line("HINT", h) }
            if let code = error.sqlstate, !error.isRefusal { Text("SQLSTATE \(code)").font(Typo.micro).foregroundStyle(.tertiary) }
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background((error.isRefusal ? Color.orange : Color.red).opacity(0.08), in: RoundedRectangle(cornerRadius: 12, style: .continuous))
    }

    private func line(_ label: String, _ text: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(label).font(mono(12, .semibold)).foregroundStyle(.secondary)
            Text(text).font(mono(12, .regular)).foregroundStyle(.secondary).textSelection(.enabled)
        }
    }

    /// The statement line containing the error position, and a caret under it.
    private var caretLines: (String, String)? {
        guard let stmt = error.statement, let pos = error.position, pos > 0, pos <= stmt.count + 1 else { return nil }
        let chars = Array(stmt)
        let idx = min(pos - 1, chars.count - 1)
        var start = idx; while start > 0 && chars[start - 1] != "\n" { start -= 1 }
        var end = idx; while end < chars.count && chars[end] != "\n" { end += 1 }
        let lineText = String(chars[start..<end])
        return (lineText, String(repeating: " ", count: idx - start) + "^")
    }
}

enum PSQLExport {
    static func tsv(_ r: SQLRunResult) -> String {
        ([r.columns.map(\.name)] + r.rows.map { $0.map { $0 ?? "" } }).map { $0.joined(separator: "\t") }.joined(separator: "\n")
    }
    static func csv(_ r: SQLRunResult) -> String {
        func q(_ s: String?) -> String {
            guard let s else { return "" }
            return s.contains(where: { $0 == "," || $0 == "\"" || $0 == "\n" }) ? "\"" + s.replacingOccurrences(of: "\"", with: "\"\"") + "\"" : s
        }
        return ([r.columns.map(\.name)] + r.rows.map { $0 }).map { $0.map(q).joined(separator: ",") }.joined(separator: "\n")
    }
    static func markdown(_ r: SQLRunResult) -> String {
        func cell(_ s: String?) -> String { (s ?? "NULL").replacingOccurrences(of: "|", with: "\\|").replacingOccurrences(of: "\n", with: " ") }
        let head = "| " + r.columns.map(\.name).joined(separator: " | ") + " |"
        let rule = "|" + r.columns.map { $0.isNumeric ? " ---: " : " --- " }.joined(separator: "|") + "|"
        return ([head, rule] + r.rows.map { "| " + $0.map(cell).joined(separator: " | ") + " |" }).joined(separator: "\n")
    }
}
