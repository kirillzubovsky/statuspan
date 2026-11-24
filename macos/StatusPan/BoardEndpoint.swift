import Foundation

/// Loopback board address. `STATUSPAN_ADDR` is `host:port`; an empty value uses the daemon default.
struct BoardEndpoint {
    let raw: String
    let host: String
    let port: Int

    init?(raw: String) {
        let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let colon = trimmed.lastIndex(of: ":") else { return nil }
        var host = String(trimmed[..<colon])
        let portText = String(trimmed[trimmed.index(after: colon)...])
        if host.hasPrefix("["), host.hasSuffix("]"), host.count >= 2 {
            host = String(host.dropFirst().dropLast())
        }
        guard !host.isEmpty, !host.contains("/"), !host.contains(where: \.isWhitespace) else { return nil }
        guard let port = Int(portText), (1...65535).contains(port) else { return nil }
        self.raw = trimmed
        self.host = host
        self.port = port
    }

    static func resolve(environment: [String: String] = ProcessInfo.processInfo.environment) -> BoardEndpoint? {
        let raw = environment["STATUSPAN_ADDR"]?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        if raw.isEmpty {
            return BoardEndpoint(raw: "127.0.0.1:4477")
        }
        return BoardEndpoint(raw: raw)
    }

    var boardURL: URL? { URL(string: "http://\(raw)/") }
    var healthURL: URL? { URL(string: "http://\(raw)/healthz") }

    func matches(_ url: URL) -> Bool {
        guard url.scheme == "http" || url.scheme == "https" else { return false }
        guard let urlHost = url.host, urlHost.caseInsensitiveCompare(host) == .orderedSame else { return false }
        let urlPort = url.port ?? (url.scheme == "https" ? 443 : 80)
        return urlPort == port
    }
}

/// GET /healthz. Any HTTP response counts as the board answering, so the app does not start a second daemon.
final class HealthClient {
    private let session: URLSession

    init() {
        let config = URLSessionConfiguration.ephemeral
        config.waitsForConnectivity = false
        config.requestCachePolicy = .reloadIgnoringLocalCacheData
        config.urlCache = nil
        config.timeoutIntervalForRequest = 15
        config.timeoutIntervalForResource = 15
        session = URLSession(configuration: config)
    }

    func answers(_ url: URL?, timeout: TimeInterval) -> Bool {
        guard let url else { return false }
        var request = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: timeout)
        request.httpMethod = "GET"
        let box = ProbeBox()
        let semaphore = DispatchSemaphore(value: 0)
        let task = session.dataTask(with: request) { _, response, _ in
            if response is HTTPURLResponse {
                box.ok = true
            }
            semaphore.signal()
        }
        task.resume()
        if semaphore.wait(timeout: .now() + timeout + 0.25) == .timedOut {
            task.cancel()
            return false
        }
        return box.ok
    }
}

private final class ProbeBox {
    var ok = false
}
