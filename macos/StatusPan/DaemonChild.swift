import Darwin
import Foundation

/// Child `statuspan serve` started by the app. A board that was already running is never tracked here.
final class DaemonChild {
    enum Launch {
        case started
        case alreadyRunning
        case failed
    }

    private let lock = NSLock()
    private var process: Process?
    private var logHandles: [FileHandle] = []
    private var startedByApp = false
    private var quitting = false

    func start(executable: URL, addr: String) -> Launch {
        lock.lock()
        defer { lock.unlock() }
        if quitting {
            return .failed
        }
        if let process, process.isRunning {
            return .alreadyRunning
        }
        guard FileManager.default.fileExists(atPath: executable.path) else {
            note("bundled daemon is missing at \(executable.path)")
            return .failed
        }

        let proc = Process()
        proc.executableURL = executable
        proc.arguments = ["serve", "--addr", addr]
        // nil does not inherit on this macOS: the child starts with an empty
        // environment (no HOME, no STATUSPAN_DATA) and a relative data dir
        // lands on cwd. Copy the app environment and pin cwd to the user home
        // so a relative fallback cannot land in / or the repo.
        proc.environment = ProcessInfo.processInfo.environment
        proc.currentDirectoryURL = FileManager.default.homeDirectoryForCurrentUser
        proc.standardInput = FileHandle.nullDevice

        let handles = openLogHandles()
        if handles.count == 2 {
            proc.standardOutput = handles[0]
            proc.standardError = handles[1]
        } else {
            proc.standardOutput = FileHandle.nullDevice
            proc.standardError = FileHandle.nullDevice
        }

        do {
            try proc.run()
        } catch {
            note("failed to start daemon: \(error)")
            return .failed
        }

        logHandles = handles
        process = proc
        startedByApp = true
        return .started
    }

    var isRunning: Bool {
        lock.lock()
        defer { lock.unlock() }
        return process?.isRunning ?? false
    }

    /// SIGTERM only the daemon this app launched, then SIGKILL if it is still alive.
    func stopIfOwned() {
        lock.lock()
        quitting = true
        let proc = startedByApp ? process : nil
        lock.unlock()
        guard let proc, proc.isRunning else { return }

        proc.terminate()
        let deadline = Date().addingTimeInterval(2)
        while proc.isRunning && Date() < deadline {
            Thread.sleep(forTimeInterval: 0.05)
        }
        guard proc.isRunning else { return }
        let pid = proc.processIdentifier
        if pid > 0 {
            _ = kill(pid, SIGKILL)
        }
        proc.waitUntilExit()
    }

    private func openLogHandles() -> [FileHandle] {
        guard let url = logURL() else { return [] }
        let directory = url.deletingLastPathComponent()
        do {
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
            if !FileManager.default.fileExists(atPath: url.path) {
                FileManager.default.createFile(atPath: url.path, contents: nil)
            }
            let out = try appendHandle(to: url)
            let err = try appendHandle(to: url)
            let stamp = ISO8601DateFormatter().string(from: Date())
            if let data = "\n--- statuspan serve \(stamp) ---\n".data(using: .utf8) {
                try out.write(contentsOf: data)
            }
            return [out, err]
        } catch {
            return []
        }
    }

    /// Opened with O_APPEND so stdout and stderr do not overwrite each other.
    private func appendHandle(to url: URL) throws -> FileHandle {
        let fd = Darwin.open(url.path, O_WRONLY | O_CREAT | O_APPEND, mode_t(0o644))
        if fd < 0 {
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
        }
        return FileHandle(fileDescriptor: fd, closeOnDealloc: true)
    }

    /// ~/Library/Logs/StatusPan/daemon.log, resolved at runtime.
    private func logURL() -> URL? {
        guard let library = FileManager.default.urls(for: .libraryDirectory, in: .userDomainMask).first else {
            return nil
        }
        return library
            .appendingPathComponent("Logs", isDirectory: true)
            .appendingPathComponent("StatusPan", isDirectory: true)
            .appendingPathComponent("daemon.log")
    }

    private func note(_ line: String) {
        guard let url = logURL() else { return }
        let directory = url.deletingLastPathComponent()
        do {
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
            if !FileManager.default.fileExists(atPath: url.path) {
                FileManager.default.createFile(atPath: url.path, contents: nil)
            }
            let handle = try appendHandle(to: url)
            let stamp = ISO8601DateFormatter().string(from: Date())
            if let data = "--- \(stamp) \(line)\n".data(using: .utf8) {
                try handle.write(contentsOf: data)
            }
        } catch {
            return
        }
    }
}
