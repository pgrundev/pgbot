import Charts
import PGBotKit
import SwiftUI

enum MetricsSection: String, CaseIterable, Identifiable {
    case overview = "Overview", queries = "Queries", connections = "Connections", locks = "Locks & Waits"
    case replication = "Replication", vacuum = "Vacuum", storage = "Storage", wal = "WAL", errors = "Errors"
    case indexes = "Indexes", schema = "Schema", settings = "Settings"
    var id: String { rawValue }
    var symbol: String {
        switch self {
        case .overview: "square.grid.2x2"
        case .queries: "bolt.horizontal"
        case .connections: "person.2"
        case .locks: "lock"
        case .replication: "arrow.triangle.2.circlepath"
        case .vacuum: "wind"
        case .storage: "internaldrive"
        case .wal: "waveform.path"
        case .errors: "exclamationmark.bubble"
        case .indexes: "list.bullet.indent"
        case .schema: "tablecells"
        case .settings: "slider.horizontal.3"
        }
    }
}

enum TimeRange: String, CaseIterable, Identifiable {
    case hour = "1H", sixHours = "6H", day = "24H", week = "7D", month = "30D"
    var id: String { rawValue }
    var seconds: TimeInterval {
        switch self { case .hour: 3600; case .sixHours: 21600; case .day: 86400; case .week: 604_800; case .month: 2_592_000 }
    }
}

@MainActor struct MetricsView: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot
    @State private var section: MetricsSection = .overview
    @State private var range: TimeRange = .day
    @State private var customStart = Date().addingTimeInterval(-86400 * 3)
    @State private var useCustom = false

    var body: some View {
        VStack(spacing: 0) {
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 4) {
                    ForEach(MetricsSection.allCases) { s in
                        Button { withAnimation(.snappy(duration: 0.2)) { section = s } } label: {
                            Text(s.rawValue)
                                .font(.sys(size: 13, weight: section == s ? .semibold : .regular))
                                .foregroundStyle(section == s ? Color.primary : Color.secondary)
                                .padding(.horizontal, 12).frame(height: 30)
                                .background(section == s ? Palette.selection : .clear, in: Capsule())
                                .contentShape(Capsule())
                        }
                        .buttonStyle(.plain)
                    }
                }
                .padding(.horizontal, 24)
            }
            .frame(maxWidth: .infinity)
            .padding(.top, 4)

            ScrollView {
                VStack(alignment: .leading, spacing: Space.l) {
                    HStack {
                        Text(section.rawValue).font(.sys(size: 24, weight: .semibold))
                        Spacer()
                        if [.overview, .connections, .storage, .wal, .replication].contains(section) {
                            Picker("Range", selection: $range) { ForEach(TimeRange.allCases) { Text($0.rawValue).tag($0) } }
                                .pickerStyle(.segmented).labelsHidden().frame(width: 230).disabled(useCustom)
                            Toggle("Custom", isOn: $useCustom).toggleStyle(.checkbox)
                            if useCustom { DatePicker("", selection: $customStart, in: ...Date()).labelsHidden().frame(width: 170) }
                        }
                    }
                    content
                }
                .frame(maxWidth: 980)
                .padding(.horizontal, 28).padding(.top, 18).padding(.bottom, 32)
                .frame(maxWidth: .infinity)
            }
        }
    }

    private var since: Date { useCustom ? customStart : Date().addingTimeInterval(-range.seconds) }
    private var ctx: EngineContext? { model.latest[bot.id]?.context }

    @ViewBuilder private var content: some View {
        if let ctx {
            switch section {
            case .overview: OverviewSection(bot: bot, ctx: ctx, record: model.latest[bot.id]!, since: since)
            case .queries: QueriesSection(bot: bot, ctx: ctx)
            case .connections: ConnectionsSection(bot: bot, ctx: ctx, since: since)
            case .locks: LocksSection(ctx: ctx)
            case .replication: ReplicationSectionView(bot: bot, ctx: ctx, since: since)
            case .vacuum: VacuumSection(ctx: ctx)
            case .storage: StorageSection(bot: bot, ctx: ctx, since: since)
            case .wal: WALSectionView(bot: bot, ctx: ctx, since: since)
            case .errors: ErrorsSection(bot: bot, ctx: ctx)
            case .indexes: IndexesSectionView(ctx: ctx)
            case .schema: SchemaSection(ctx: ctx)
            case .settings: SettingsParamsSection(ctx: ctx)
            }
        } else {
            ContentUnavailableView {
                Label("No metrics yet", systemImage: "chart.xyaxis.line")
            } description: {
                Text("Metrics appear after the first inspection. History builds up as PGBot keeps monitoring.")
            } actions: {
                Button("Run inspection") { Task { await model.inspect(bot.id, reason: .manual) } }
            }
            .padding(.top, 60)
        }
    }
}

// MARK: - Building blocks

@MainActor struct MetricTile: View {
    let title: String
    let value: String?
    var caption: String? = nil
    var tint: Color = .primary
    var unavailableReason: String? = nil
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title).font(.caption).foregroundStyle(.secondary)
            if let value {
                Text(value).font(.title2.weight(.semibold).monospacedDigit()).foregroundStyle(tint).lineLimit(1).minimumScaleFactor(0.7)
            } else {
                Text("Unavailable").font(.title3).foregroundStyle(.tertiary)
            }
            if let c = value == nil ? unavailableReason : caption {
                Text(c).font(.caption2).foregroundStyle(.tertiary).lineLimit(2)
            }
        }
        .frame(maxWidth: .infinity, minHeight: 72, alignment: .topLeading)
        .card(padding: Space.m)
    }
}

@MainActor struct HistoryChart: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot
    let metric: MetricKey
    let since: Date
    var format: (Double) -> String = { String(format: "%.0f", $0) }

    var body: some View {
        let points = (try? model.store.samples(for: bot.id, metric: metric, since: since)) ?? []
        VStack(alignment: .leading, spacing: Space.s) {
            HStack {
                Text(metric.label).font(.headline)
                Spacer()
                if let last = points.last { Text(format(last.value)).font(.callout.monospacedDigit()).foregroundStyle(.secondary) }
            }
            if points.count < 2 {
                Text(points.isEmpty ? "No history in this range yet." : "One sample so far — the trend appears after the next inspection.")
                    .font(.callout).foregroundStyle(.tertiary).frame(maxWidth: .infinity, minHeight: 110)
            } else {
                Chart(points, id: \.at) { p in
                    AreaMark(x: .value("Time", p.at), y: .value(metric.label, p.value))
                        .foregroundStyle(LinearGradient(colors: [bot.avatar.color.color.opacity(0.22), .clear], startPoint: .top, endPoint: .bottom))
                        .interpolationMethod(.monotone)
                    LineMark(x: .value("Time", p.at), y: .value(metric.label, p.value))
                        .foregroundStyle(bot.avatar.color.color).lineStyle(.init(lineWidth: 1.5)).interpolationMethod(.monotone)
                }
                .chartYAxis { AxisMarks(position: .leading) { v in
                    AxisGridLine().foregroundStyle(Palette.hairline)
                    AxisValueLabel { if let d = v.as(Double.self) { Text(format(d)).font(.caption2) } }
                } }
                .chartXAxis { AxisMarks { _ in AxisGridLine().foregroundStyle(.clear); AxisValueLabel().font(.caption2) } }
                .chartOverlay { proxy in ChartHover(proxy: proxy, points: points, format: format) }
                .frame(height: 140)
            }
        }
        .card(padding: Space.m)
    }
}

/// Tooltip with timestamp and value under the pointer.
@MainActor struct ChartHover: View {
    let proxy: ChartProxy
    let points: [MetricPoint]
    let format: (Double) -> String
    @State private var hover: MetricPoint?
    var body: some View {
        GeometryReader { g in
            Rectangle().fill(.clear).contentShape(Rectangle())
                .onContinuousHover { phase in
                    switch phase {
                    case .active(let loc):
                        guard let plot = proxy.plotFrame, let date: Date = proxy.value(atX: loc.x - g[plot].origin.x) else { return }
                        hover = points.min { abs($0.at.timeIntervalSince(date)) < abs($1.at.timeIntervalSince(date)) }
                    case .ended: hover = nil
                    }
                }
                .overlay(alignment: .topLeading) {
                    if let h = hover, let plot = proxy.plotFrame, let x = proxy.position(forX: h.at) {
                        let px = g[plot].origin.x + x
                        Rectangle().fill(Color.secondary.opacity(0.4)).frame(width: 1, height: g[plot].height).offset(x: px, y: g[plot].origin.y)
                        VStack(alignment: .leading, spacing: 1) {
                            Text(format(h.value)).font(.caption.weight(.semibold).monospacedDigit())
                            Text(h.at.formatted(date: .abbreviated, time: .shortened)).font(.caption2).foregroundStyle(.secondary)
                        }
                        .padding(6).background(.regularMaterial, in: RoundedRectangle(cornerRadius: 6))
                        .offset(x: min(max(0, px - 50), g.size.width - 130), y: 0)
                    }
                }
        }
    }
}

@MainActor struct UnavailableNote: View {
    let title: String
    let reason: String
    var body: some View {
        Label { VStack(alignment: .leading, spacing: 2) { Text(title).font(.headline); Text(reason).foregroundStyle(.secondary) } }
            icon: { Image(systemName: "eye.slash").foregroundStyle(.secondary) }
            .frame(maxWidth: .infinity, alignment: .leading)
            .card(padding: Space.m)
    }
}

let tileColumns = [GridItem(.adaptive(minimum: 170), spacing: Space.m)]

// MARK: - Sections

@MainActor struct OverviewSection: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot
    let ctx: EngineContext
    let record: InspectionRecord
    let since: Date
    var body: some View {
        VStack(alignment: .leading, spacing: Space.l) {
            HealthScoreView(score: record.score, previous: model.previous[bot.id]?.score.value,
                            inspectedAt: record.finishedAt, bands: model.settings.bands).card()
            LazyVGrid(columns: tileColumns, spacing: Space.m) {
                MetricTile(title: "Active connections", value: ctx.activity?.active.map { "\($0)" }, caption: ctx.activity?.total.map { "\($0) total sessions" })
                MetricTile(title: "Connection utilization",
                           value: ctx.limits.map { "\($0.connectionsUsed)/\($0.connectionsMax)" },
                           caption: ctx.limits?.utilization.map { Format.percent($0, digits: 0) + " of max_connections" })
                MetricTile(title: "Transactions / sec", value: ctx.health?.tps.map { String(format: "%.1f", $0) }, unavailableReason: ctx.health?.reason)
                MetricTile(title: "Avg query time",
                           value: avgQueryMS.map(Format.ms), caption: "Across pg_stat_statements",
                           unavailableReason: ctx.queries?.reason ?? "Needs pg_stat_statements")
                MetricTile(title: "Cache hit ratio", value: ctx.health?.cacheHitRatio.map { Format.percent($0) },
                           tint: (ctx.health?.cacheHitRatio ?? 1) < 0.95 ? .orange : .primary)
                MetricTile(title: "Database size", value: ctx.tables?.dbSizeBytes.map(Format.bytes))
                MetricTile(title: "Deadlocks / min", value: ctx.health?.deadlocksPerMin.map { String(format: "%.2f", $0) })
                MetricTile(title: "Rollback rate", value: ctx.health?.rollbackRatio.map { Format.percent($0, digits: 2) })
                MetricTile(title: "Replication lag", value: replicationLag, unavailableReason: "No replicas reporting")
                MetricTile(title: "WAL generation", value: ctx.wal?.bytesPerSec.map { Format.bytes($0) + "/s" }, unavailableReason: ctx.wal?.reason)
                MetricTile(title: "Autovacuum", value: ctx.activity?.autovacuumWorkers.map { "\($0) running" } ?? "Idle",
                           caption: ctx.findings.contains { $0.id.contains("vacuum") } ? "See Vacuum" : "No vacuum findings")
                MetricTile(title: "Findings", value: "\(record.score.critical) critical · \(record.score.warnings) warn",
                           tint: record.score.critical > 0 ? .red : (record.score.warnings > 0 ? .orange : .primary))
            }
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 340), spacing: Space.m)], spacing: Space.m) {
                HistoryChart(bot: bot, metric: .healthScore, since: since)
                HistoryChart(bot: bot, metric: .connections, since: since)
                HistoryChart(bot: bot, metric: .tps, since: since) { String(format: "%.0f", $0) }
                HistoryChart(bot: bot, metric: .cacheHitRatio, since: since) { Format.percent($0) }
            }
            UnavailableNote(title: "Infrastructure metrics (CPU, memory, disk, network)",
                            reason: "PGBot reads PostgreSQL over SQL only, so host metrics aren't measured here — and never estimated. Provider integrations (RDS/CloudWatch, Cloud SQL, Azure Monitor) will supply them in a future release.")
        }
    }

    private var avgQueryMS: Double? {
        guard let top = ctx.queries?.top, !top.isEmpty, ctx.queries?.isAvailable == true else { return nil }
        let calls = top.reduce(0) { $0 + $1.calls }
        return calls > 0 ? top.reduce(0) { $0 + $1.totalMS } / Double(calls) : nil
    }

    private var replicationLag: String? {
        let lags = (ctx.replication?.replicas ?? []).compactMap(\.replayLagSec) + [ctx.replication?.receiverLagSec].compactMap { $0 }
        return lags.max().map(Format.seconds)
    }
}

@MainActor struct QueriesSection: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot
    let ctx: EngineContext
    @State private var sortOrder = [KeyPathComparator(\QueryStat.totalMS, order: .reverse)]
    @State private var filter = ""
    @State private var selected: QueryStat.ID?

    var body: some View {
        if let q = ctx.queries, q.isAvailable, let top = q.top {
            let rows = top.filter { filter.isEmpty || $0.query.localizedCaseInsensitiveContains(filter) }.sorted(using: sortOrder)
            VStack(alignment: .leading, spacing: Space.m) {
                TextField("Filter queries", text: $filter).textFieldStyle(.roundedBorder).frame(maxWidth: 300)
                Table(rows, selection: $selected, sortOrder: $sortOrder) {
                    TableColumn("Query", value: \.query) { Text(Format.sqlPreview($0.query, limit: 120)).font(.system(.caption, design: .monospaced)) }
                        .width(min: 260, ideal: 420)
                    TableColumn("Total", value: \.totalMS) { Text(Format.ms($0.totalMS)).monospacedDigit() }.width(80)
                    TableColumn("Mean", value: \.meanMS) { Text(Format.ms($0.meanMS)).monospacedDigit() }.width(70)
                    TableColumn("Calls", value: \.calls) { Text(Format.count($0.calls)).monospacedDigit() }.width(70)
                    TableColumn("Cache hit") { Text($0.cacheHit.map { Format.percent($0) } ?? "—").monospacedDigit() }.width(70)
                }
                .frame(minHeight: 300)
                if let id = selected, let qs = top.first(where: { $0.id == id }) {
                    VStack(alignment: .leading, spacing: Space.s) {
                        SQLBlock(sql: qs.query)
                        HStack {
                            Text("\(Format.count(qs.calls)) calls · mean \(Format.ms(qs.meanMS)) · max \(Format.ms(qs.maxMS ?? 0)) · \(Format.count(qs.rows ?? 0)) rows")
                                .font(.callout).foregroundStyle(.secondary)
                            Spacer()
                            Button("Explain SQL") { model.route = .chat; model.explain(sql: qs.query, on: bot.id) }
                                .disabled(!qs.query.lowercased().hasPrefix("select"))
                            Button("Ask PGBot") { model.route = .chat; model.send("Why is this query expensive, and what can I do about it? queryid \(qs.queryID)", to: bot.id) }
                                .buttonStyle(.borderedProminent)
                        }
                    }
                    .card(padding: Space.m)
                }
                Text("Normalized statements from pg_stat_statements; literals are replaced with placeholders.")
                    .font(.caption).foregroundStyle(.tertiary)
            }
        } else {
            UnavailableNote(title: "Query statistics unavailable",
                            reason: (ctx.queries?.reason ?? "pg_stat_statements isn't enabled.") + " Enable the pg_stat_statements extension to see expensive and slow queries.")
        }
    }
}

@MainActor struct ConnectionsSection: View {
    let bot: DatabaseBot
    let ctx: EngineContext
    let since: Date
    var body: some View {
        VStack(alignment: .leading, spacing: Space.l) {
            LazyVGrid(columns: tileColumns, spacing: Space.m) {
                MetricTile(title: "Total", value: ctx.activity?.total.map { "\($0)" })
                MetricTile(title: "Active", value: ctx.activity?.active.map { "\($0)" })
                MetricTile(title: "Idle", value: ctx.activity?.idle.map { "\($0)" })
                MetricTile(title: "Idle in transaction", value: ctx.activity?.idleInTransaction.map { "\($0)" },
                           caption: ctx.activity?.longestIdleXactSec.map { "Longest " + Format.seconds($0) },
                           tint: (ctx.activity?.idleInTransaction ?? 0) > 0 ? .orange : .primary)
                MetricTile(title: "Longest transaction", value: ctx.activity?.longestXactSec.map(Format.seconds))
                MetricTile(title: "max_connections", value: ctx.limits.map { "\($0.connectionsMax)" })
            }
            HistoryChart(bot: bot, metric: .connections, since: since)
            if let groups = ctx.activity?.connections, !groups.isEmpty {
                TableCard(title: "Top connection sources", columns: ["Application", "User", "State", "Count"],
                          rows: groups.map { [$0.appName.isEmpty ? "—" : $0.appName, $0.user, $0.state, "\($0.count)"] })
            }
        }
    }
}

@MainActor struct LocksSection: View {
    let ctx: EngineContext
    var body: some View {
        VStack(alignment: .leading, spacing: Space.l) {
            LazyVGrid(columns: tileColumns, spacing: Space.m) {
                MetricTile(title: "Blocked sessions", value: ctx.locks?.blockedCount.map { "\($0)" },
                           tint: (ctx.locks?.blockedCount ?? 0) > 0 ? .orange : .primary, unavailableReason: ctx.locks?.reason)
                MetricTile(title: "Waiting sessions", value: ctx.activity?.waiting.map { "\($0)" })
                MetricTile(title: "Idle in transaction", value: ctx.activity?.idleInTransaction.map { "\($0)" })
                MetricTile(title: "Deadlocks / min", value: ctx.health?.deadlocksPerMin.map { String(format: "%.2f", $0) })
            }
            if let chains = ctx.locks?.chains, !chains.isEmpty {
                TableCard(title: "Blocking relationships", columns: ["Blocked", "Blocked by", "Wait", "Waiting", "Query"],
                          rows: chains.map { ["\($0.blockedPID)", $0.blockingPIDs.map(String.init).joined(separator: ", "), $0.waitEvent ?? "", Format.seconds($0.waitSeconds), Format.sqlPreview($0.blockedQuery)] })
            } else {
                Label("No blocking chains at the last inspection.", systemImage: "checkmark.circle").foregroundStyle(.secondary)
            }
            if let wp = ctx.waitProfile {
                if wp.available, let buckets = wp.buckets, !buckets.isEmpty {
                    VStack(alignment: .leading, spacing: Space.s) {
                        Text("Where time went (\(wp.samples ?? 0) samples over \(Format.seconds(wp.windowSeconds ?? 0)))").font(.headline)
                        Chart(buckets, id: \.type) { b in
                            BarMark(x: .value("Share", b.share), y: .value("Wait", b.type))
                                .annotation(position: .trailing) { Text(Format.percent(b.share, digits: 0)).font(.caption2).foregroundStyle(.secondary) }
                        }
                        .chartXAxis(.hidden)
                        .frame(height: CGFloat(buckets.count) * 26 + 10)
                    }
                    .card(padding: Space.m)
                } else {
                    UnavailableNote(title: "Wait sampling unavailable", reason: wp.reason ?? "No samples captured.")
                }
            }
        }
    }
}

@MainActor struct ReplicationSectionView: View {
    let bot: DatabaseBot
    let ctx: EngineContext
    let since: Date
    var body: some View {
        VStack(alignment: .leading, spacing: Space.l) {
            if let r = ctx.replication, r.isAvailable {
                LazyVGrid(columns: tileColumns, spacing: Space.m) {
                    MetricTile(title: "Role", value: r.isReplica == true ? "Standby" : "Primary")
                    MetricTile(title: "Replicas", value: "\(r.replicas?.count ?? 0)")
                    MetricTile(title: "Slots", value: "\(r.slots?.count ?? 0)", caption: "\(r.slots?.filter { !$0.active }.count ?? 0) inactive",
                               tint: (r.slots?.contains { !$0.active } ?? false) ? .orange : .primary)
                    MetricTile(title: "Retained WAL", value: Format.bytes((r.slots ?? []).reduce(0) { $0 + $1.retainedBytes }))
                }
                HistoryChart(bot: bot, metric: .replicationLagSec, since: since) { Format.seconds($0) }
                if let reps = r.replicas, !reps.isEmpty {
                    TableCard(title: "Replicas", columns: ["Name", "State", "Sync", "Replay lag", "Bytes behind"],
                              rows: reps.map { [$0.appName ?? $0.clientAddr, $0.state, $0.syncState, $0.replayLagSec.map(Format.seconds) ?? "idle", Format.bytes($0.replayLagBytes ?? 0)] })
                }
                if let slots = r.slots, !slots.isEmpty {
                    TableCard(title: "Replication slots", columns: ["Slot", "Type", "Active", "Retained", "WAL status"],
                              rows: slots.map { [$0.name, $0.type, $0.active ? "yes" : "no", Format.bytes($0.retainedBytes), $0.walStatus ?? "—"] })
                }
            } else {
                UnavailableNote(title: "Replication unavailable", reason: ctx.replication?.reason ?? "Not collected.")
            }
        }
    }
}

@MainActor struct VacuumSection: View {
    let ctx: EngineContext
    var body: some View {
        VStack(alignment: .leading, spacing: Space.l) {
            LazyVGrid(columns: tileColumns, spacing: Space.m) {
                MetricTile(title: "Transaction ID age", value: ctx.limits?.maxXIDAge.map { Format.count($0) },
                           caption: ctx.limits?.wraparoundFraction.map { Format.percent($0) + " of the wraparound limit" },
                           tint: (ctx.limits?.wraparoundFraction ?? 0) > 0.5 ? .orange : .primary)
                MetricTile(title: "Autovacuum workers", value: ctx.activity?.autovacuumWorkers.map { "\($0)" } ?? "0")
                MetricTile(title: "Dead tuples (top tables)", value: Format.count((ctx.tables?.top ?? []).reduce(0) { $0 + $1.deadTuples }))
            }
            let tables = (ctx.tables?.top ?? []).sorted { $0.deadRatio > $1.deadRatio }
            if !tables.isEmpty {
                TableCard(title: "Tables", columns: ["Table", "Dead", "Dead %", "Last vacuum", "Last analyze"],
                          rows: tables.map { [$0.id, Format.count($0.deadTuples), Format.percent($0.deadRatio),
                                              $0.lastAnyVacuum.map { $0.formatted(.relative(presentation: .named)) } ?? "never",
                                              [$0.lastAnalyze, $0.lastAutoanalyze].compactMap { $0 }.max().map { $0.formatted(.relative(presentation: .named)) } ?? "never"] })
            }
            let vf = ctx.findings.filter { ($0.id.contains("vacuum") || $0.id.contains("bloat") || $0.id.contains("wraparound")) && !$0.isSuppressed }
            ForEach(vf, id: \.key) { FindingLine(finding: $0) }
        }
    }
}

@MainActor struct StorageSection: View {
    let bot: DatabaseBot
    let ctx: EngineContext
    let since: Date
    var body: some View {
        VStack(alignment: .leading, spacing: Space.l) {
            LazyVGrid(columns: tileColumns, spacing: Space.m) {
                MetricTile(title: "Database size", value: ctx.tables?.dbSizeBytes.map(Format.bytes))
                MetricTile(title: "Potentially unused indexes", value: "\(ctx.indexes?.unused?.count ?? 0)",
                           caption: Format.bytes((ctx.indexes?.unused ?? []).reduce(0) { $0 + $1.bytes }))
            }
            HistoryChart(bot: bot, metric: .dbSizeBytes, since: since) { Format.bytes($0) }
            if let top = ctx.tables?.top, !top.isEmpty {
                VStack(alignment: .leading, spacing: Space.s) {
                    Text("Largest tables").font(.headline)
                    Chart(top.prefix(10)) { t in
                        BarMark(x: .value("Size", t.totalBytes), y: .value("Table", t.id))
                            .foregroundStyle(bot.avatar.color.color.opacity(0.8))
                            .annotation(position: .trailing) { Text(Format.bytes(t.totalBytes)).font(.caption2).foregroundStyle(.secondary) }
                    }
                    .chartXAxis(.hidden).frame(height: CGFloat(min(top.count, 10)) * 26 + 10)
                }
                .card(padding: Space.m)
            }
            Text("Sizes include indexes and TOAST storage (pg_total_relation_size).").font(.caption).foregroundStyle(.tertiary)
        }
    }
}

@MainActor struct WALSectionView: View {
    let bot: DatabaseBot
    let ctx: EngineContext
    let since: Date
    var body: some View {
        VStack(alignment: .leading, spacing: Space.l) {
            LazyVGrid(columns: tileColumns, spacing: Space.m) {
                MetricTile(title: "WAL generation", value: ctx.wal?.bytesPerSec.map { Format.bytes($0) + "/s" }, unavailableReason: ctx.wal?.reason)
                MetricTile(title: "pg_wal directory", value: ctx.wal?.dirBytes.map(Format.bytes), unavailableReason: "Needs pg_monitor (pg_ls_waldir)")
            }
            HistoryChart(bot: bot, metric: .walBytesPerSec, since: since) { Format.bytes($0) + "/s" }
        }
    }
}

@MainActor struct ErrorsSection: View {
    @Environment(AppModel.self) private var model
    let bot: DatabaseBot
    let ctx: EngineContext
    var body: some View {
        VStack(alignment: .leading, spacing: Space.l) {
            LazyVGrid(columns: tileColumns, spacing: Space.m) {
                MetricTile(title: "Rollback rate", value: ctx.health?.rollbackRatio.map { Format.percent($0, digits: 2) })
                MetricTile(title: "Deadlocks / min", value: ctx.health?.deadlocksPerMin.map { String(format: "%.2f", $0) })
            }
            UnavailableNote(title: "Error events come from the server log",
                            reason: "PostgreSQL statistics count rollbacks and deadlocks, but not individual errors, timeouts or failed logins. The Logs view reads them over SQL when the monitoring role can read the log; managed providers like Supabase and Neon expose logs through their own APIs.")
            Button("Open Logs") { model.route = .logs }.buttonStyle(.bordered)
        }
    }
}

@MainActor struct IndexesSectionView: View {
    let ctx: EngineContext
    var body: some View {
        VStack(alignment: .leading, spacing: Space.l) {
            if let ix = ctx.indexes, ix.isAvailable {
                LazyVGrid(columns: tileColumns, spacing: Space.m) {
                    MetricTile(title: "Indexes", value: "\(ix.total ?? 0)")
                    MetricTile(title: "Potentially unused", value: "\(ix.unused?.count ?? 0)", caption: Format.bytes((ix.unused ?? []).reduce(0) { $0 + $1.bytes }))
                }
                if let u = ix.unused, !u.isEmpty {
                    TableCard(title: "Potentially unused indexes", columns: ["Index", "Table", "Size", "Scans"],
                              rows: u.map { [$0.id, $0.table, Format.bytes($0.bytes), Format.count($0.scans)] })
                    Text("Zero scans since statistics were last reset. Check every replica before removing an index — PGBot never drops anything.")
                        .font(.caption).foregroundStyle(.secondary)
                }
                if let l = ix.largest, !l.isEmpty {
                    TableCard(title: "Largest indexes", columns: ["Index", "Table", "Size", "Scans"],
                              rows: l.map { [$0.id, $0.table, Format.bytes($0.bytes), Format.count($0.scans)] })
                }
            } else {
                UnavailableNote(title: "Index statistics unavailable", reason: ctx.indexes?.reason ?? "Not collected.")
            }
        }
    }
}

@MainActor struct SchemaSection: View {
    let ctx: EngineContext
    var body: some View {
        VStack(alignment: .leading, spacing: Space.l) {
            if let t = ctx.tables?.top, !t.isEmpty {
                TableCard(title: "Tables (largest first)", columns: ["Table", "Rows", "Size", "Seq scans", "Index scans"],
                          rows: t.map { [$0.id, Format.count($0.liveTuples), Format.bytes($0.totalBytes), Format.count($0.seqScans ?? 0), Format.count($0.indexScans ?? 0)] })
            }
            Text("For a full ER diagram, run `pgbot erd` in a terminal.").font(.caption).foregroundStyle(.tertiary)
        }
    }
}

@MainActor struct SettingsParamsSection: View {
    let ctx: EngineContext
    var body: some View {
        if let s = ctx.settings, s.isAvailable {
            VStack(alignment: .leading, spacing: Space.l) {
                if let p = s.params, !p.isEmpty {
                    TableCard(title: "Tuning parameters", columns: ["Setting", "Value"], rows: p.sorted { $0.key < $1.key }.map { [$0.key, $0.value] })
                }
                if let o = s.overrides, !o.isEmpty {
                    TableCard(title: "Changed from defaults", columns: ["Setting", "Value"], rows: o.sorted { $0.key < $1.key }.map { [$0.key, $0.value] })
                }
            }
        } else {
            UnavailableNote(title: "Settings unavailable", reason: ctx.settings?.reason ?? "Not collected.")
        }
    }
}
