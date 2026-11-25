import AppKit
import Foundation

struct T3BridgeSummary: Decodable {
    let enabled: Bool
    let available: Bool
    let state: String
    let binary: String?
    let pendingQuestions: Int
    let trackedQuestions: Int
    let lastWakeAt: String?
    let lastWakeThread: String?
    let lastWakeIds: [Int]?
    let lastError: String?
}

struct WorkerVMSummary: Decodable {
    let configured: Bool
    let hostname: String?
    let arch: String?
    let cpus: Int?
    let memoryMB: Int?
    let autostart: Bool
    let state: String
    let detail: String?
}

struct DaemonSnapshot: Decodable {
    let version: String
    let build: Int
    let pid: Int
    let controller: String?
    let cockpitUrl: String?
    let logPath: String?
    let t3Bridge: T3BridgeSummary
    let workerVm: WorkerVMSummary
}

func makeDot(color: NSColor) -> NSImage {
    let size = NSSize(width: 9, height: 9)
    let img = NSImage(size: size, flipped: false) { rect in
        color.setFill()
        NSBezierPath(ovalIn: rect.insetBy(dx: 0.5, dy: 0.5)).fill()
        return true
    }
    img.isTemplate = false
    return img
}

func shortHost(_ raw: String?) -> String {
    guard let raw = raw, !raw.isEmpty else { return "not configured" }
    if let url = URL(string: raw), let host = url.host {
        return host
    }
    return raw
}

func truncate(_ s: String, maxLen: Int) -> String {
    if s.count <= maxLen { return s }
    return String(s.prefix(maxLen - 1)) + "…"
}

func relativeTime(_ iso: String?) -> String? {
    guard let iso = iso, !iso.isEmpty else { return nil }
    let f1 = ISO8601DateFormatter()
    f1.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    let f2 = ISO8601DateFormatter()
    f2.formatOptions = [.withInternetDateTime]
    guard let date = f1.date(from: iso) ?? f2.date(from: iso) else { return nil }
    let secs = max(0, Int(Date().timeIntervalSince(date)))
    if secs < 60 { return "\(secs)s ago" }
    if secs < 3600 { return "\(secs / 60)m ago" }
    if secs < 86400 { return "\(secs / 3600)h ago" }
    return "\(secs / 86400)d ago"
}

@MainActor
final class HotbedMenubarDelegate: NSObject, NSApplicationDelegate {
    private var statusItem: NSStatusItem!
    private var menu: NSMenu!
    private var latest: DaemonSnapshot?

    func applicationDidFinishLaunching(_ notification: Notification) {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        menu = NSMenu()
        menu.autoenablesItems = false
        statusItem.menu = menu
        render(nil)
        startReadingStdin()
    }

    private func sendAction(_ action: String) {
        let line = "{\"action\":\"\(action)\"}\n"
        if let data = line.data(using: .utf8) {
            FileHandle.standardOutput.write(data)
        }
    }

    @objc private func openCockpit() { sendAction("open_cockpit") }
    @objc private func openLogs() { sendAction("open_logs") }
    @objc private func startVM() { sendAction("start_vm") }
    @objc private func stopVM() { sendAction("stop_vm") }
    @objc private func restartDaemon() { sendAction("restart") }

    private func updateIcon(_ snap: DaemonSnapshot?) {
        guard let button = statusItem.button else { return }
        var symbolName = "flame"
        var tooltip = "Hotbed"

        if let snap = snap {
            let vmErr = snap.workerVm.state == "error" || snap.workerVm.state == "paused"
            let bridgeConnected = snap.t3Bridge.state == "connected"
            let vmRunning = snap.workerVm.state == "running"

            if vmErr {
                symbolName = "exclamationmark.triangle.fill"
            } else if bridgeConnected || vmRunning {
                symbolName = "flame.fill"
            } else {
                symbolName = "flame"
            }

            var parts: [String] = ["Hotbed \(snap.version)"]
            if snap.workerVm.configured, let h = snap.workerVm.hostname {
                parts.append("\(h): \(snap.workerVm.state)")
            }
            if snap.t3Bridge.enabled {
                parts.append("T3 bridge: \(snap.t3Bridge.state)")
            }
            tooltip = parts.joined(separator: " · ")

            if snap.t3Bridge.pendingQuestions > 0 {
                button.title = " \(snap.t3Bridge.pendingQuestions)"
            } else {
                button.title = ""
            }
        } else {
            button.title = ""
        }

        if let img = NSImage(systemSymbolName: symbolName, accessibilityDescription: "Hotbed") {
            img.isTemplate = true
            button.image = img
            button.imagePosition = button.title.isEmpty ? .imageOnly : .imageLeading
        } else {
            button.title = "Hotbed"
        }
        button.toolTip = tooltip
    }

    private func infoItem(_ title: String, dot: NSColor? = nil) -> NSMenuItem {
        let item = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        item.isEnabled = false
        if let dot = dot {
            item.image = makeDot(color: dot)
        }
        return item
    }

    private func sectionHeader(_ title: String) -> NSMenuItem {
        if #available(macOS 14.0, *) {
            return NSMenuItem.sectionHeader(title: title)
        }
        return infoItem(title)
    }

    private func render(_ snap: DaemonSnapshot?) {
        latest = snap
        updateIcon(snap)
        menu.removeAllItems()

        guard let snap = snap else {
            menu.addItem(infoItem("Hotbed — Starting…", dot: .systemYellow))
            return
        }

        // Header
        let ver = snap.version.isEmpty ? "Hotbed" : "Hotbed  (\(snap.version))"
        menu.addItem(infoItem(ver))
        menu.addItem(infoItem("Controller: \(shortHost(snap.controller))"))
        menu.addItem(NSMenuItem.separator())

        // T3 Code Bridge section
        menu.addItem(sectionHeader("T3 Code Bridge"))
        let b = snap.t3Bridge
        let (bridgeLabel, bridgeColor): (String, NSColor) = {
            switch b.state {
            case "connected":
                if b.pendingQuestions > 0 {
                    return ("Connected · \(b.pendingQuestions) pending", .systemYellow)
                }
                return ("Connected", .systemGreen)
            case "reconnecting":
                return ("Reconnecting…", .systemOrange)
            case "disabled":
                return ("Disabled", .systemGray)
            default:
                return ("Unavailable", .systemGray)
            }
        }()
        menu.addItem(infoItem(bridgeLabel, dot: bridgeColor))
        if b.trackedQuestions > 0 {
            let noun = b.trackedQuestions == 1 ? "question" : "questions"
            menu.addItem(infoItem("Notified: \(b.trackedQuestions) \(noun)"))
        }
        if let ids = b.lastWakeIds, !ids.isEmpty {
            let idList = ids.map { "#\($0)" }.joined(separator: ", ")
            if let ago = relativeTime(b.lastWakeAt) {
                menu.addItem(infoItem("Last wake: \(idList) (\(ago))"))
            } else {
                menu.addItem(infoItem("Last wake: \(idList)"))
            }
        }
        if let err = b.lastError, !err.isEmpty {
            menu.addItem(infoItem(truncate(err, maxLen: 48)))
        }

        menu.addItem(NSMenuItem.separator())

        // Local Worker VM section
        menu.addItem(sectionHeader("Local Worker VM"))
        let vm = snap.workerVm
        if vm.configured {
            let name = vm.hostname ?? "vm"
            let (vmLabel, vmColor): (String, NSColor) = {
                switch vm.state {
                case "running": return ("\(name): Running", .systemGreen)
                case "starting": return ("\(name): Starting…", .systemYellow)
                case "stopping": return ("\(name): Stopping…", .systemYellow)
                case "asleep": return ("\(name): Asleep (Standby)", .systemBlue)
                case "paused": return ("\(name): Paused", .systemOrange)
                case "error": return ("\(name): Error", .systemRed)
                default: return ("\(name): Stopped", .systemGray)
                }
            }()
            menu.addItem(infoItem(vmLabel, dot: vmColor))
            if let cpus = vm.cpus, let memMB = vm.memoryMB, cpus > 0, memMB > 0 {
                let gb = Double(memMB) / 1024.0
                let memStr = gb.truncatingRemainder(dividingBy: 1) == 0 ? "\(Int(gb)) GB" : String(format: "%.1f GB", gb)
                let arch = vm.arch ?? ""
                menu.addItem(infoItem("\(cpus) vCPUs · \(memStr) RAM\(arch.isEmpty ? "" : " · \(arch)")"))
            }
            if let detail = vm.detail, !detail.isEmpty {
                menu.addItem(infoItem(truncate(detail, maxLen: 48)))
            }

            if vm.state == "running" || vm.state == "paused" {
                let stopItem = NSMenuItem(title: "Stop Worker VM", action: #selector(stopVM), keyEquivalent: "")
                stopItem.target = self
                menu.addItem(stopItem)
            } else if vm.state == "starting" || vm.state == "stopping" {
                menu.addItem(infoItem(vm.state == "starting" ? "Starting Worker VM…" : "Stopping Worker VM…"))
            } else {
                let startItem = NSMenuItem(title: "Start Worker VM", action: #selector(startVM), keyEquivalent: "")
                startItem.target = self
                menu.addItem(startItem)
            }
        } else {
            menu.addItem(infoItem("No local VM configured", dot: .systemGray))
        }

        menu.addItem(NSMenuItem.separator())

        // Actions
        if let url = snap.cockpitUrl, !url.isEmpty {
            let cockpit = NSMenuItem(title: "Open Cockpit", action: #selector(openCockpit), keyEquivalent: "")
            cockpit.target = self
            menu.addItem(cockpit)
        }
        if let logPath = snap.logPath, !logPath.isEmpty {
            let logs = NSMenuItem(title: "View Daemon Logs", action: #selector(openLogs), keyEquivalent: "")
            logs.target = self
            menu.addItem(logs)
        }
        let restart = NSMenuItem(title: "Restart Daemon", action: #selector(restartDaemon), keyEquivalent: "")
        restart.target = self
        menu.addItem(restart)
    }

    private func startReadingStdin() {
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let decoder = JSONDecoder()
            while let line = readLine(strippingNewline: true) {
                guard let data = line.data(using: .utf8) else { continue }
                if let snap = try? decoder.decode(DaemonSnapshot.self, from: data) {
                    DispatchQueue.main.async {
                        self?.render(snap)
                    }
                }
            }
            // Parent hotbed daemon exited (EOF on stdin) -> exit cleanly.
            DispatchQueue.main.async {
                NSApp.terminate(nil)
            }
        }
    }
}

MainActor.assumeIsolated {
    let app = NSApplication.shared
    app.setActivationPolicy(.accessory)
    let delegate = HotbedMenubarDelegate()
    app.delegate = delegate
    app.run()
}
