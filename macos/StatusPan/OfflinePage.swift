import Foundation

enum OfflinePage {
    static func html(kicker: String, title: String, message: String, showRetry: Bool) -> String {
        let retry = showRetry ? retryButton : ""
        return """
        <!DOCTYPE html>
        <html lang="en">
        <head>
        <meta charset="utf-8">
        <meta name="viewport" content="width=device-width, initial-scale=1">
        <meta name="color-scheme" content="dark">
        <title>StatusPan</title>
        <style>
          :root { color-scheme: dark; }
          * { box-sizing: border-box; }
          html, body { margin: 0; height: 100%; background: #1b1c20; color: #e4e5e9; }
          body {
            font: 14px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif;
            -webkit-font-smoothing: antialiased;
          }
          header {
            min-height: 50px; display: flex; align-items: center; gap: 10px;
            padding: 0 20px; background: #222329; border-bottom: 1px solid #2f3138;
            font-weight: 650; letter-spacing: -0.01em;
          }
          .mark {
            width: 20px; height: 20px; border-radius: 6px; background: #f0a35c;
            display: inline-flex; align-items: center; justify-content: center; flex: none;
          }
          .mark i { width: 8px; height: 8px; border-radius: 50%; border: 2px solid #222329; display: block; }
          .sh {
            height: 40px; display: flex; align-items: center; padding: 0 20px;
            font-size: 11.5px; font-weight: 700; letter-spacing: 0.14em; text-transform: uppercase;
            color: #f0a35c; background: #242228; border-bottom: 1px solid #2f3138;
          }
          main { padding: 28px 20px; max-width: 34rem; }
          h1 { margin: 0 0 8px; font-size: 22px; font-weight: 650; letter-spacing: -0.02em; }
          p { margin: 0 0 18px; color: #a7aab2; }
          button {
            appearance: none; height: 32px; padding: 0 14px; border: 0; border-radius: 6px;
            background: #f0a35c; color: #1b1c20; font: inherit; font-weight: 700; cursor: pointer;
          }
          button:hover { filter: brightness(1.06); }
          button:focus-visible { outline: 2px solid #f0a35c; outline-offset: 3px; }
        </style>
        </head>
        <body>
        <header><span class="mark"><i></i></span> StatusPan</header>
        <div class="sh">\(escape(kicker))</div>
        <main>
          <h1>\(escape(title))</h1>
          <p>\(escape(message))</p>
          \(retry)
        </main>
        </body>
        </html>
        """
    }

    private static let retryButton = """
    <button type="button" id="retry">Retry</button>
    <script>
    document.getElementById("retry").addEventListener("click", function () {
      try {
        window.webkit.messageHandlers.retry.postMessage("retry");
      } catch (e) {
        window.location.href = "statuspan-app://retry";
      }
    });
    </script>
    """

    private static func escape(_ text: String) -> String {
        let clipped = text.count > 240 ? String(text.prefix(240)) + "…" : text
        return clipped
            .replacingOccurrences(of: "&", with: "&amp;")
            .replacingOccurrences(of: "<", with: "&lt;")
            .replacingOccurrences(of: ">", with: "&gt;")
            .replacingOccurrences(of: "\"", with: "&quot;")
    }
}
