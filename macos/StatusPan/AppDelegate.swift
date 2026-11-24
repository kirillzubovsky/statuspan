import AppKit
import WebKit

private enum ConnectResult {
    case ready
    case offline(String)
    case stale
}

/// Forwards Retry clicks without retaining the delegate. WKUserContentController retains its handlers.
final class RetryHandler: NSObject, WKScriptMessageHandler {
    weak var owner: AppDelegate?

    init(owner: AppDelegate) {
        self.owner = owner
    }

    func userContentController(_ userContentController: WKUserContentController, didReceive message: WKScriptMessage) {
        guard message.name == "retry" else { return }
        owner?.connect()
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate, WKNavigationDelegate, WKUIDelegate {
    private let daemon = DaemonChild()
    private let health = HealthClient()
    private let workQueue = DispatchQueue(label: "com.statuspan.app.board")
    private let tokenLock = NSLock()

    private var window: NSWindow?
    private var webView: WKWebView?
    private var endpoint: BoardEndpoint?
    private var publishedToken = 0
    private var latestToken = 0
    private var appliedGeneration = 0
    private var showingOffline = false
    private var lastExternalOpen: (url: URL, at: Date)?

    func applicationDidFinishLaunching(_ notification: Notification) {
        endpoint = BoardEndpoint.resolve()
        makeWindow()
        if #available(macOS 14, *) {
            NSApp.activate()
        } else {
            NSApp.activate(ignoringOtherApps: true)
        }
        if endpoint == nil {
            showOffline("STATUSPAN_ADDR must be a host:port address, for example 127.0.0.1:4477.")
            return
        }
        connect()
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        true
    }

    func applicationSupportsSecureRestorableState(_ app: NSApplication) -> Bool {
        true
    }

    func applicationWillTerminate(_ notification: Notification) {
        daemon.stopIfOwned()
    }

    func connect() {
        dispatchPrecondition(condition: .onQueue(.main))
        guard let endpoint else {
            showOffline("STATUSPAN_ADDR must be a host:port address, for example 127.0.0.1:4477.")
            return
        }
        let token = bumpToken()
        showingOffline = false
        show(
            kicker: "Board",
            title: "Starting",
            message: "Looking for the board at \(endpoint.raw).",
            retry: false
        )
        let daemonURL = Bundle.main.resourceURL?.appendingPathComponent("statuspan")
        let health = self.health
        let daemon = self.daemon
        workQueue.async { [weak self] in
            let result = Self.performConnect(
                endpoint: endpoint,
                daemonURL: daemonURL,
                health: health,
                daemon: daemon,
                isCurrent: { self?.isCurrent(token) ?? false }
            )
            DispatchQueue.main.async {
                guard let self, self.isCurrent(token) else { return }
                switch result {
                case .ready:
                    self.loadBoard(token: token)
                case .offline(let message):
                    self.showOffline(message)
                case .stale:
                    break
                }
            }
        }
    }

    @objc func reloadPage(_ sender: Any?) {
        if let webView, let url = webView.url, endpoint?.matches(url) == true {
            webView.reload()
            return
        }
        connect()
    }

    @objc func zoomIn(_ sender: Any?) {
        guard let webView else { return }
        webView.pageZoom = min(webView.pageZoom * 1.2, 3)
    }

    @objc func zoomOut(_ sender: Any?) {
        guard let webView else { return }
        webView.pageZoom = max(webView.pageZoom / 1.2, 0.5)
    }

    @objc func actualSize(_ sender: Any?) {
        webView?.pageZoom = 1
    }

    func webView(
        _ webView: WKWebView,
        decidePolicyFor navigationAction: WKNavigationAction,
        decisionHandler: @escaping (WKNavigationActionPolicy) -> Void
    ) {
        guard let url = navigationAction.request.url else {
            decisionHandler(.allow)
            return
        }
        if url.scheme == "statuspan-app" {
            let retry = url.host == "retry"
            decisionHandler(.cancel)
            if retry {
                connect()
            }
            return
        }
        if isHTTP(url), endpoint?.matches(url) != true {
            openExternal(url)
            decisionHandler(.cancel)
            return
        }
        if isHTTP(url), navigationAction.targetFrame == nil {
            decisionHandler(.cancel)
            webView.load(URLRequest(url: url))
            return
        }
        decisionHandler(.allow)
    }

    func webView(
        _ webView: WKWebView,
        createWebViewWith configuration: WKWebViewConfiguration,
        for navigationAction: WKNavigationAction,
        windowFeatures: WKWindowFeatures
    ) -> WKWebView? {
        guard let url = navigationAction.request.url, isHTTP(url) else { return nil }
        if endpoint?.matches(url) == true {
            webView.load(URLRequest(url: url))
        } else {
            openExternal(url)
        }
        return nil
    }

    func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
        handleNavigationFailure(error)
    }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        handleNavigationFailure(error)
    }

    private func makeWindow() {
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 1200, height: 800),
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered,
            defer: false
        )
        window.title = "StatusPan"
        window.minSize = NSSize(width: 640, height: 420)
        window.isReleasedWhenClosed = false
        window.tabbingMode = .disallowed
        let background = nsColor(red: 0x1b, green: 0x1c, blue: 0x20)
        window.backgroundColor = background
        window.center()
        window.setFrameAutosaveName("StatusPanMainWindow")

        let config = WKWebViewConfiguration()
        config.preferences.javaScriptCanOpenWindowsAutomatically = true
        config.userContentController.add(RetryHandler(owner: self), name: "retry")
        let webView = WKWebView(frame: window.contentView?.bounds ?? .zero, configuration: config)
        webView.navigationDelegate = self
        webView.uiDelegate = self
        webView.underPageBackgroundColor = background
        webView.allowsBackForwardNavigationGestures = false
        webView.autoresizingMask = [.width, .height]
        window.contentView?.addSubview(webView)

        self.window = window
        self.webView = webView
        window.makeKeyAndOrderFront(nil)
    }

    private func loadBoard(token: Int) {
        guard let url = endpoint?.boardURL, let webView else {
            showOffline("The board address is not a valid URL.")
            return
        }
        showingOffline = false
        appliedGeneration = token
        var request = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 10)
        request.httpMethod = "GET"
        webView.load(request)
    }

    private func showOffline(_ message: String) {
        showingOffline = true
        appliedGeneration = latestToken
        show(kicker: "Offline", title: "The board is not answering", message: message, retry: true)
    }

    private func show(kicker: String, title: String, message: String, retry: Bool) {
        let html = OfflinePage.html(kicker: kicker, title: title, message: message, showRetry: retry)
        webView?.loadHTMLString(html, baseURL: nil)
    }

    private func handleNavigationFailure(_ error: Error) {
        dispatchPrecondition(condition: .onQueue(.main))
        if isIgnorable(error) || showingOffline || appliedGeneration != latestToken {
            return
        }
        let whereText = endpoint?.raw ?? "the configured address"
        showOffline("The board at \(whereText) did not load.")
    }

    private func openExternal(_ url: URL) {
        if let last = lastExternalOpen, last.url == url, Date().timeIntervalSince(last.at) < 0.6 {
            return
        }
        lastExternalOpen = (url, Date())
        NSWorkspace.shared.open(url)
    }

    private func bumpToken() -> Int {
        latestToken += 1
        let token = latestToken
        tokenLock.lock()
        publishedToken = token
        tokenLock.unlock()
        return token
    }

    private func isCurrent(_ token: Int) -> Bool {
        tokenLock.lock()
        defer { tokenLock.unlock() }
        return publishedToken == token
    }

    private static func performConnect(
        endpoint: BoardEndpoint,
        daemonURL: URL?,
        health: HealthClient,
        daemon: DaemonChild,
        isCurrent: () -> Bool
    ) -> ConnectResult {
        if !isCurrent() { return .stale }
        if health.answers(endpoint.healthURL, timeout: 1) {
            return .ready
        }
        if !isCurrent() { return .stale }
        guard let daemonURL else {
            return .offline("StatusPan could not find the bundled board.")
        }
        let startedNow: Bool
        switch daemon.start(executable: daemonURL, addr: endpoint.raw) {
        case .failed:
            return .offline("StatusPan could not start the local board. Nothing is answering at \(endpoint.raw).")
        case .started:
            startedNow = true
        case .alreadyRunning:
            startedNow = false
        }
        let deadline = Date().addingTimeInterval(10)
        while Date() < deadline {
            if !isCurrent() { return .stale }
            if health.answers(endpoint.healthURL, timeout: 0.5) {
                return .ready
            }
            if startedNow && !daemon.isRunning {
                break
            }
            Thread.sleep(forTimeInterval: 0.2)
        }
        if !isCurrent() { return .stale }
        return .offline("The board at \(endpoint.raw) did not come up.")
    }

    private func isHTTP(_ url: URL) -> Bool {
        url.scheme == "http" || url.scheme == "https"
    }

    private func isIgnorable(_ error: Error) -> Bool {
        let ns = error as NSError
        if ns.domain == NSURLErrorDomain && ns.code == NSURLErrorCancelled {
            return true
        }
        // Policy cancels (external links, target=_blank) report this WebKit code.
        if ns.domain == WKErrorDomain && ns.code == 102 {
            return true
        }
        return false
    }

    private func nsColor(red: CGFloat, green: CGFloat, blue: CGFloat) -> NSColor {
        NSColor(srgbRed: red / 255, green: green / 255, blue: blue / 255, alpha: 1)
    }
}
