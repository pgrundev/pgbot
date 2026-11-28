import AppKit
import Charts
import PGBotKit
import SwiftUI

/// One message. PGBot speaks in soft gray bubbles (no avatar — the bot is in
/// the header); the user's messages sit on the right in a slightly darker
/// bubble. Copy/regenerate appear on hover.
@MainActor struct MessageView: View {
    @Environment(AppModel.self) private var model
    let message: ChatMessage
    let bot: DatabaseBot
    @State private var hovering = false

    var body: some View {
        Group {
            switch message.role {
            case .user: userBubble
            case .system: systemEvent
            case .assistant: assistant
            }
        }
        .onHover { hovering = $0 }
    }

    /// Bot bubbles hug their content up to ~78% of the column, user bubbles ~70% (spec).
    static let botMaxWidth: CGFloat = readableWidth * 0.78
    static let userMaxWidth: CGFloat = readableWidth * 0.70

    private var userBubble: some View {
        let compact = model.settings.density == .compact
        return VStack(alignment: .trailing, spacing: 4) {
            if case .sql(_, let sql) = message.payload {
                SQLBlock(sql: sql).frame(maxWidth: Self.userMaxWidth)
            } else if case .psqlInput(let input) = message.payload {
                PSQLInputBubble(input: input, database: bot.connection.database).frame(maxWidth: readableWidth * 0.85, alignment: .trailing)
            } else {
                Text(message.plainText)
                    .font(Typo.body).lineSpacing(2).textSelection(.enabled)
                    .foregroundStyle(Palette.userText)
                    .padding(.horizontal, compact ? 14 : 16).padding(.vertical, compact ? 8 : 10)
                    .background(Palette.userBubble, in: RoundedRectangle(cornerRadius: 20, style: .continuous))
                    .frame(maxWidth: Self.userMaxWidth, alignment: .trailing)
            }
            MessageActions(message: message).opacity(hovering ? 1 : 0)
        }
        .frame(maxWidth: .infinity, alignment: .trailing)
        .help(message.createdAt.formatted(date: .abbreviated, time: .shortened))
        .contextMenu { copyItem }
    }

    @ViewBuilder private var systemEvent: some View {
        HStack(spacing: 6) {
            switch message.payload {
            case .connectionEvent(let s, let ok):
                Image(systemName: ok ? "checkmark.circle.fill" : "xmark.circle.fill").foregroundStyle(ok ? .green : .red)
                Text(s)
            default:
                Text(message.plainText)
            }
        }
        .font(Typo.small).foregroundStyle(.secondary)
        .frame(maxWidth: .infinity)
    }

    private var assistant: some View {
        let plain = model.settings.bubbleStyle == .plain
        let compact = model.settings.density == .compact
        let isCard: Bool = { if case .text = message.payload { return false }; return true }()
        let wide: Bool = { if case .psqlResult = message.payload { return true }; return false }()
        return VStack(alignment: .leading, spacing: 4) {
            content
                .font(Typo.body)
                .padding(.horizontal, plain ? 2 : (compact ? 14 : (isCard ? 18 : 16)))
                .padding(.vertical, plain ? 2 : (compact ? 9 : (isCard ? 16 : 11)))
                .background(plain ? .clear : Palette.bubble, in: RoundedRectangle(cornerRadius: 20, style: .continuous))
                .frame(maxWidth: wide ? .infinity : Self.botMaxWidth, alignment: .leading)
            HStack(spacing: 6) {
                MessageActions(message: message)
                if let m = message.modelName, m != "demo" { Text(m).font(Typo.micro).foregroundStyle(.tertiary) }
            }
            .padding(.leading, 8)
            .opacity(hovering ? 1 : 0)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .help(message.createdAt.formatted(date: .abbreviated, time: .shortened))
        .contextMenu { copyItem }
    }

    @ViewBuilder private var content: some View {
        switch message.payload {
        case .text(let s): MarkdownText(source: s)
        case .healthReport(let r): HealthReportCard(report: r, bot: bot)
        case .finding(let f, let change): FindingCard(finding: f, change: change, bot: bot, standalone: true)
        case .findingsList(let title, let fs):
            VStack(alignment: .leading, spacing: 10) {
                Text(title).font(Typo.bodyEmphasis)
                ForEach(fs, id: \.key) { FindingCard(finding: $0, change: nil, bot: bot) }
            }
        case .sql(let title, let sql):
            VStack(alignment: .leading, spacing: 10) { Text(title).font(Typo.bodyEmphasis); SQLBlock(sql: sql) }
        case .plan(let sql, let reading):
            VStack(alignment: .leading, spacing: 10) {
                if !sql.isEmpty { SQLBlock(sql: sql) }
                PlanBlock(text: reading)
            }
        case .table(let title, let cols, let rows): TableCard(title: title, columns: cols, rows: rows, framed: false)
        case .metric(let key, let points): MiniChart(metric: key, points: points)
        case .resolved(let fs):
            VStack(alignment: .leading, spacing: 8) {
                Text("✅ Issue resolved").font(Typo.bodyEmphasis)
                ForEach(fs, id: \.key) { Text($0.title).foregroundStyle(Palette.muted) }
            }
        case .error(let title, let detail, let hint): ErrorContent(title: title, detail: detail, hint: hint, message: message)
        case .connectionEvent(let s, _), .monitoringEvent(let s): Text(s).foregroundStyle(.secondary)
        case .psqlInput(let s): Text(s).font(.sys(size: 13.5, design: .monospaced))
        case .psqlResult(let r): PSQLResultView(payload: r)
        }
    }

    private var copyItem: some View { Button("Copy Message") { Pasteboard.copy(message.plainText) } }
}

@MainActor struct MessageActions: View {
    @Environment(AppModel.self) private var model
    let message: ChatMessage
    @State private var copied = false
    var body: some View {
        HStack(spacing: 2) {
            small(copied ? "checkmark" : "doc.on.doc", "Copy") {
                Pasteboard.copy(message.plainText); copied = true
                DispatchQueue.main.asyncAfter(deadline: .now() + 1.2) { copied = false }
            }
            if message.role == .assistant && message.inReplyTo != nil {
                small("arrow.clockwise", "Regenerate") { model.regenerate(message) }
                    .disabled(model.generating[message.botID] != nil)
            }
        }
    }
    private func small(_ s: String, _ help: String, _ a: @escaping () -> Void) -> some View {
        Button(action: a) { Image(systemName: s).font(.sys(size: 11.5)).frame(width: 24, height: 20) }
            .buttonStyle(.plain).foregroundStyle(.secondary).hoverHighlight(cornerRadius: 6).help(help).accessibilityLabel(help)
    }
}

enum Pasteboard {
    static func copy(_ s: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(s, forType: .string)
    }
}

// MARK: - Markdown

/// Block-level Markdown: paragraphs, headings, lists and fenced code (SQL is
/// highlighted). Inline styling uses Foundation's Markdown parser.
@MainActor struct MarkdownText: View {
    let source: String

    enum Block: Hashable { case heading(Int, String), paragraph(String), bullet(String, String), code(String, String) }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            ForEach(Array(blocks.enumerated()), id: \.offset) { _, b in
                switch b {
                case .heading(let level, let s):
                    inline(s).font(level <= 2 ? Typo.title : Typo.bodyEmphasis).padding(.top, 2)
                case .paragraph(let s): inline(s)
                case .bullet(let marker, let s):
                    HStack(alignment: .firstTextBaseline, spacing: 8) {
                        Text(marker).foregroundStyle(.secondary).frame(minWidth: 12, alignment: .trailing)
                        inline(s)
                    }
                case .code(let lang, let s):
                    if lang.isEmpty || lang.lowercased() == "sql" { SQLBlock(sql: s) } else { PlanBlock(text: s) }
                }
            }
        }
        .lineSpacing(3)
        .textSelection(.enabled)
    }

    private func inline(_ s: String) -> Text {
        guard var a = try? AttributedString(markdown: s, options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace)) else { return Text(s) }
        // `code` spans (table and column names) read as links-colored identifiers, per the spec.
        for run in a.runs where run.inlinePresentationIntent?.contains(.code) == true {
            a[run.range].foregroundColor = Palette.link
        }
        return Text(a)
    }

    var blocks: [Block] {
        var out: [Block] = []
        var para: [String] = []
        var inCode = false, lang = "", code: [String] = []
        func flush() { if !para.isEmpty { out.append(.paragraph(para.joined(separator: " "))); para = [] } }
        for raw in source.components(separatedBy: "\n") {
            let line = raw.trimmingCharacters(in: .whitespaces)
            if line.hasPrefix("```") {
                if inCode { out.append(.code(lang, code.joined(separator: "\n"))); code = []; inCode = false }
                else { flush(); inCode = true; lang = String(line.dropFirst(3)) }
                continue
            }
            if inCode { code.append(raw); continue }
            if line.isEmpty { flush(); continue }
            if let m = line.firstMatch(of: #/^(#{1,6})\s+(.*)$/#) { flush(); out.append(.heading(m.1.count, String(m.2))); continue }
            if let m = line.firstMatch(of: #/^[-*•]\s+(.*)$/#) { flush(); out.append(.bullet("•", String(m.1))); continue }
            if let m = line.firstMatch(of: #/^(\d+)[.)]\s+(.*)$/#) { flush(); out.append(.bullet("\(m.1).", String(m.2))); continue }
            para.append(line)
        }
        if inCode { out.append(.code(lang, code.joined(separator: "\n"))) }
        flush()
        return out
    }
}

// MARK: - SQL & plans

@MainActor struct SQLBlock: View {
    let sql: String
    @State private var copied = false
    var body: some View {
        ZStack(alignment: .topTrailing) {
            ScrollView(.horizontal, showsIndicators: false) {
                Text(SQLHighlighter.highlight(sql)).font(.sys(size: 13, design: .monospaced))
                    .lineSpacing(2).textSelection(.enabled).padding(14).padding(.trailing, 28)
            }
            Button {
                Pasteboard.copy(sql); copied = true
                DispatchQueue.main.asyncAfter(deadline: .now() + 1.2) { copied = false }
            } label: {
                Image(systemName: copied ? "checkmark" : "doc.on.doc").font(.sys(size: 11.5)).padding(7)
            }
            .buttonStyle(.plain).foregroundStyle(.secondary).help("Copy SQL").accessibilityLabel("Copy SQL").padding(4)
        }
        .background(Palette.inset, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
    }
}

enum SQLHighlighter {
    static let keywords: Set<String> = [
        "select", "from", "where", "and", "or", "not", "in", "is", "null", "join", "left", "right", "inner", "outer", "on",
        "group", "by", "order", "having", "limit", "offset", "as", "distinct", "insert", "into", "values", "update", "set",
        "delete", "create", "index", "table", "drop", "alter", "add", "concurrently", "if", "exists", "with", "union", "all",
        "case", "when", "then", "else", "end", "asc", "desc", "returning", "grant", "revoke", "role", "to", "vacuum", "analyze",
        "explain", "begin", "commit", "rollback", "like", "ilike", "between", "count", "sum", "avg", "min", "max", "now",
    ]

    static func highlight(_ sql: String) -> AttributedString {
        var out = AttributedString()
        let pattern = #/('(?:[^']|'')*')|(--[^\n]*)|(\$\d+)|(\b\d+(?:\.\d+)?\b)|([A-Za-z_][A-Za-z0-9_]*)|(\s+)|(.)/#
        for m in sql.matches(of: pattern) {
            var piece = AttributedString(String(m.output.0))
            if m.output.1 != nil { piece.foregroundColor = .init(nsColor: .systemRed) }
            else if m.output.2 != nil { piece.foregroundColor = .init(nsColor: .secondaryLabelColor) }
            else if m.output.3 != nil || m.output.4 != nil { piece.foregroundColor = .init(nsColor: .systemOrange) }
            else if let w = m.output.5, keywords.contains(w.lowercased()) { piece.foregroundColor = .init(nsColor: .systemPurple) }
            out += piece
        }
        return out
    }
}

@MainActor struct PlanBlock: View {
    let text: String
    var body: some View {
        ScrollView(.horizontal) {
            Text(text.trimmingCharacters(in: .whitespacesAndNewlines))
                .font(.sys(size: 12, design: .monospaced)).textSelection(.enabled)
                .padding(14).fixedSize(horizontal: true, vertical: false)
        }
        .frame(maxHeight: 420)
        .background(Palette.inset, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
        .overlay(alignment: .topTrailing) {
            Button { Pasteboard.copy(text) } label: { Image(systemName: "doc.on.doc").font(.sys(size: 11.5)).padding(9) }
                .buttonStyle(.plain).foregroundStyle(.secondary).help("Copy")
        }
    }
}

// MARK: - Structured content

@MainActor struct HealthReportCard: View {
    @Environment(AppModel.self) private var model
    let report: HealthReportPayload
    let bot: DatabaseBot
    @State private var showDetails = false

    var body: some View {
        let notable = report.findings.filter { $0.severity >= .warn }
        let band = model.settings.bands.band(for: report.score)
        VStack(alignment: .leading, spacing: 12) {
            Text("🐘 " + title).font(Typo.bodyEmphasis)
            Text(report.headline)

            HStack(alignment: .firstTextBaseline, spacing: 10) {
                if report.score.completeness == .unavailable {
                    Text("—").font(.sys(size: 34, weight: .semibold))
                    Text("Health score unavailable").font(.sys(size: 14)).foregroundStyle(Palette.muted)
                } else {
                    Text("\(report.score.value)/100")
                        .font(.sys(size: 34, weight: .semibold)).kerning(-0.5)
                        .foregroundStyle(band.color)
                    Text("Health score").font(.sys(size: 14)).foregroundStyle(Palette.muted)
                    if let prev = report.previousScore, prev != report.score.value {
                        let d = report.score.value - prev
                        Text("\(d > 0 ? "+" : "")\(d) since last check").font(.sys(size: 13)).foregroundStyle(d > 0 ? Palette.healthy : .orange)
                    }
                    if report.score.completeness == .partial {
                        Text("Incomplete").font(.sys(size: 11, weight: .semibold))
                            .padding(.horizontal, 6).padding(.vertical, 2)
                            .background(Color.orange.opacity(0.16), in: Capsule()).foregroundStyle(.orange)
                            .help(report.score.gaps.joined(separator: "\n"))
                    }
                }
            }

            Rectangle().fill(Palette.hairline).frame(height: 1)

            if showDetails {
                ForEach(report.findings, id: \.key) { FindingCard(finding: $0, change: nil, bot: bot, standalone: false) }
            } else {
                ForEach(notable.prefix(5), id: \.key) { f in
                    Text("\(f.severity == .critical ? "🔴" : "🟠") \(f.isTentative ? "Possibly: " : "")\(f.title)")
                }
                if notable.count > 5 { Text("+\(notable.count - 5) more").foregroundStyle(Palette.muted) }
                if notable.isEmpty && report.score.completeness != .unavailable {
                    Text("No warnings or critical findings.").foregroundStyle(Palette.muted)
                }
            }
            if report.score.completeness != .complete && !report.score.gaps.isEmpty {
                Text("Some statistics weren't visible: " + report.score.gaps.joined(separator: " · "))
                    .font(.sys(size: 13)).foregroundStyle(Palette.muted)
            }

            HStack(spacing: 8) {
                if let top = notable.first, !bot.isDemo {
                    Button("Investigate") { model.send("Investigate: \(top.title). What's the evidence, and what should I do first?", to: bot.id) }
                        .buttonStyle(.pillProminent)
                    Button(showDetails ? "Hide details" : "Show details") { withAnimation(.snappy) { showDetails.toggle() } }
                        .buttonStyle(.pill)
                } else {
                    Button(showDetails ? "Hide details" : "Show details") { withAnimation(.snappy) { showDetails.toggle() } }
                        .buttonStyle(.pillProminent)
                }
                Button("View metrics") { withAnimation(.snappy) { model.route = .metrics } }.buttonStyle(.pill)
            }
            .padding(.top, 2)
        }
    }

    private var title: String {
        if report.isScheduled { return "Your daily database report" }
        if report.isFirstRun == true { return "Your database is connected!" }
        return "Health report"
    }
}

@MainActor struct FindingLine: View {
    let finding: Finding
    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Image(systemName: finding.severity.symbol).foregroundStyle(finding.severity.color).font(.sys(size: 13))
            Text((finding.isTentative ? "Possibly: " : "") + finding.title)
        }
    }
}

/// A finding. Standalone (a proactive message) it fills the bubble like the
/// spec's "⚠️ Performance regression" card; nested in a report it is an inset card.
@MainActor struct FindingCard: View {
    @Environment(AppModel.self) private var model
    let finding: Finding
    let change: FindingChange?
    let bot: DatabaseBot
    var standalone = false
    @State private var expanded = false

    var body: some View {
        let content = VStack(alignment: .leading, spacing: standalone ? 12 : 8) {
            if standalone {
                Text("\(finding.severity == .critical ? "🚨" : "⚠️") \(heading)").font(Typo.bodyEmphasis)
                Text((finding.isTentative ? "Possibly: " : "") + finding.title)
            } else {
                Text("\(finding.severity == .critical ? "🔴" : (finding.severity == .warn ? "🟠" : "🔵")) \(finding.isTentative ? "Possibly: " : "")\(finding.title)")
                    .font(Typo.bodyEmphasis)
            }
            if !finding.detail.isEmpty { Text(finding.detail).foregroundStyle(Palette.muted).textSelection(.enabled) }
            if let r = finding.remediation, !r.isEmpty { Text(r) }
            if let est = finding.impact?.estimate, !est.isEmpty {
                Text("Impact: \(est)").font(.sys(size: 13)).foregroundStyle(Palette.muted)
            }
            ForEach(finding.caveats ?? [], id: \.self) { c in
                Text("⚠︎ " + c).font(.sys(size: 13)).foregroundStyle(.orange)
            }
            ForEach(finding.safety?.blockingCaveats ?? [], id: \.id) { g in
                VStack(alignment: .leading, spacing: 2) {
                    Text("✋ Before \(g.action): \(g.text)").font(.sys(size: 13, weight: .medium))
                    if let v = g.verify { Text("Verify first: \(v)").font(.sys(size: 13)).foregroundStyle(Palette.muted) }
                }
                .foregroundStyle(.red)
                .padding(10).background(Color.red.opacity(0.08), in: RoundedRectangle(cornerRadius: 10))
            }
            if let ev = finding.evidence, !ev.isEmpty {
                DisclosureGroup(isExpanded: $expanded) {
                    VStack(alignment: .leading, spacing: 3) {
                        ForEach(Array(ev.prefix(30).enumerated()), id: \.offset) { _, e in
                            Text(e).font(.sys(size: 12, design: .monospaced)).textSelection(.enabled)
                        }
                        if ev.count > 30 { Text("+\(ev.count - 30) more").font(Typo.small).foregroundStyle(.secondary) }
                    }
                    .padding(.top, 4)
                } label: {
                    Text("Evidence (\(ev.count))").font(Typo.small).foregroundStyle(.secondary)
                }
            }
            HStack(spacing: 8) {
                if !bot.isDemo {
                    Button("Investigate") { model.send("Explain the finding “\(finding.title)”: why it matters here and what to do.", to: bot.id) }
                        .buttonStyle(standalone ? .pillProminent : PillButtonStyle(prominent: true, compact: true))
                }
                if finding.impact?.dimension == "latency" || finding.id.contains("quer") {
                    Button("View affected queries") { withAnimation(.snappy) { model.route = .metrics } }
                        .buttonStyle(standalone ? .pill : .pillSmall)
                }
                Button("Ignore") { model.mutate(bot.id) { $0.schedule.ignoredFindingKeys.insert(finding.key) } }
                    .buttonStyle(standalone ? .pill : .pillSmall)
                    .help("Hide this finding for this database in the app (does not change the engine's ignore rules)")
            }
            .padding(.top, 2)
        }
        if standalone { content } else { content.insetCard(padding: 14) }
    }

    private var heading: String {
        switch change {
        case .escalated: return "Escalated to \(finding.severity.label.lowercased())"
        case .deescalated: return "Improved"
        case .resolved: return "Resolved"
        default:
            if finding.severity == .critical { return "Critical finding" }
            if finding.impact?.dimension == "latency" || finding.id.contains("regress") { return "Performance regression" }
            return "New warning"
        }
    }
}

@MainActor struct TableCard: View {
    let title: String
    let columns: [String]
    let rows: [[String]]
    var framed = true
    var body: some View {
        let content = VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text(title).font(Typo.bodyEmphasis)
                Spacer()
                Button { Pasteboard.copy(([columns] + rows).map { $0.joined(separator: "\t") }.joined(separator: "\n")) } label: {
                    Image(systemName: "doc.on.doc").font(.sys(size: 11.5))
                }.buttonStyle(.plain).foregroundStyle(.secondary).help("Copy as TSV")
            }
            ScrollView(.horizontal) {
                Grid(alignment: .leading, horizontalSpacing: 18, verticalSpacing: 7) {
                    GridRow { ForEach(columns, id: \.self) { Text($0).font(Typo.micro.weight(.semibold)).foregroundStyle(.secondary) } }
                    ForEach(Array(rows.enumerated()), id: \.offset) { _, row in
                        GridRow {
                            ForEach(Array(row.enumerated()), id: \.offset) { i, cell in
                                Text(cell)
                                    .font(i == row.count - 1 && cell.count > 30 ? .sys(size: 12, design: .monospaced) : .sys(size: 13).monospacedDigit())
                                    .lineLimit(2).textSelection(.enabled)
                                    .frame(maxWidth: i == row.count - 1 ? 420 : nil, alignment: .leading)
                            }
                        }
                    }
                }
            }
        }
        if framed { content.card(padding: 14) } else { content }
    }
}

@MainActor struct MiniChart: View {
    let metric: MetricKey
    let points: [MetricPoint]
    var body: some View {
        VStack(alignment: .leading) {
            Text(metric.label).font(Typo.bodyEmphasis)
            Chart(points, id: \.at) {
                AreaMark(x: .value("Time", $0.at), y: .value(metric.label, $0.value)).opacity(0.2)
                LineMark(x: .value("Time", $0.at), y: .value(metric.label, $0.value))
            }
            .frame(height: 120)
        }
    }
}

@MainActor struct ErrorContent: View {
    @Environment(AppModel.self) private var model
    let title: String, detail: String, hint: String?
    let message: ChatMessage
    @State private var showDetail = false
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Label(title, systemImage: "exclamationmark.triangle.fill").font(Typo.bodyEmphasis).foregroundStyle(.orange)
            if let hint { Text(hint) }
            if !detail.isEmpty {
                DisclosureGroup(isExpanded: $showDetail) {
                    Text(detail).font(.sys(size: 12, design: .monospaced)).foregroundStyle(.secondary).textSelection(.enabled)
                } label: { Text("Details").font(Typo.small).foregroundStyle(.secondary) }
            }
            HStack(spacing: 8) {
                if message.inReplyTo != nil { Button("Retry") { model.regenerate(message) } }
                else { Button("Inspect again") { Task { await model.inspect(message.botID, reason: .manual) } } }
                if title.contains("AI") || title.contains("credentials") || title.contains("permissions") {
                    Button("Open Settings") { openAppSettings() }
                }
            }
            .buttonStyle(.pill)
        }
    }
}
