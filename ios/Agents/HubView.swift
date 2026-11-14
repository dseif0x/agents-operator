import SwiftUI
import UIKit
import WebKit

/// The hub in a full-screen WKWebView. The web app handles the safe areas
/// itself (viewport-fit=cover, env(safe-area-inset-*)), like the installed
/// PWA, so the web view spans the whole screen.
struct HubView: UIViewControllerRepresentable {
    let url: URL

    func makeUIViewController(context: Context) -> HubViewController {
        HubViewController(home: url)
    }

    func updateUIViewController(_ controller: HubViewController, context: Context) {}
}

final class HubViewController: UIViewController {
    private let home: URL
    private var webView: WKWebView!
    private var downloads: [WKDownload: URL] = [:]

    init(home: URL) {
        self.home = home
        super.init(nibName: nil, bundle: nil)
    }

    required init?(coder: NSCoder) { fatalError("not used") }

    override func loadView() {
        let config = WKWebViewConfiguration()
        // The default store persists cookies, so the login survives restarts.
        config.websiteDataStore = .default()
        config.allowsInlineMediaPlayback = true
        config.mediaTypesRequiringUserActionForPlayback = []
        config.applicationNameForUserAgent = "Version/18.0 Mobile/15E148 Safari/604.1 AgentsApp/1.0"

        let webView = WKWebView(frame: .zero, configuration: config)
        webView.navigationDelegate = self
        webView.uiDelegate = self
        webView.allowsBackForwardNavigationGestures = true
        webView.allowsLinkPreview = false
        webView.isOpaque = false
        webView.backgroundColor = UIColor(Color.hubBackground)
        webView.underPageBackgroundColor = UIColor(Color.hubBackground)
        webView.scrollView.backgroundColor = UIColor(Color.hubBackground)
        webView.scrollView.contentInsetAdjustmentBehavior = .never
        if #available(iOS 16.4, *) {
            webView.isInspectable = true // Safari → Develop, for debugging
        }
        self.webView = webView
        view = webView
    }

    override func viewDidLoad() {
        super.viewDidLoad()
        webView.load(URLRequest(url: home))
    }

    override var preferredStatusBarStyle: UIStatusBarStyle { .lightContent }

    fileprivate func isHub(_ url: URL?) -> Bool {
        guard let host = url?.host?.lowercased(), let homeHost = home.host?.lowercased() else { return false }
        return host == homeHost
    }

    fileprivate func openOutside(_ url: URL) {
        UIApplication.shared.open(url)
    }

    fileprivate func share(fileAt url: URL) {
        let sheet = UIActivityViewController(activityItems: [url], applicationActivities: nil)
        sheet.popoverPresentationController?.sourceView = view
        sheet.popoverPresentationController?.sourceRect = CGRect(x: view.bounds.midX, y: view.bounds.maxY - 40, width: 1, height: 1)
        topPresenter.present(sheet, animated: true)
    }

    fileprivate var topPresenter: UIViewController {
        var top: UIViewController = self
        while let next = top.presentedViewController { top = next }
        return top
    }

    private func showError(_ error: Error) {
        let ns = error as NSError
        // Cancelled loads (a new navigation replaced the old one) are normal.
        if ns.domain == NSURLErrorDomain && ns.code == NSURLErrorCancelled { return }
        if ns.domain == "WebKitErrorDomain" && ns.code == 102 { return } // frame load interrupted (downloads)
        let alert = UIAlertController(title: "Can't reach the hub", message: error.localizedDescription, preferredStyle: .alert)
        alert.addAction(UIAlertAction(title: "Retry", style: .default) { [weak self] _ in
            guard let self else { return }
            self.webView.load(URLRequest(url: self.webView.url ?? self.home))
        })
        alert.addAction(UIAlertAction(title: "Cancel", style: .cancel))
        topPresenter.present(alert, animated: true)
    }
}

// MARK: - Navigation

extension HubViewController: WKNavigationDelegate {
    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        if action.shouldPerformDownload {
            decisionHandler(.download)
            return
        }
        guard let url = action.request.url else {
            decisionHandler(.cancel)
            return
        }
        let scheme = url.scheme?.lowercased() ?? ""
        if scheme != "http" && scheme != "https" && scheme != "about" && scheme != "blob" && scheme != "data" {
            openOutside(url) // mailto:, tel:, …
            decisionHandler(.cancel)
            return
        }
        // Links to other sites open in Safari; the hub stays in the app.
        if action.targetFrame?.isMainFrame == true, (scheme == "http" || scheme == "https"), !isHub(url) {
            openOutside(url)
            decisionHandler(.cancel)
            return
        }
        decisionHandler(.allow)
    }

    func webView(_ webView: WKWebView, decidePolicyFor response: WKNavigationResponse,
                 decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
        decisionHandler(response.canShowMIMEType ? .allow : .download)
    }

    func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
        download.delegate = self
    }

    func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
        download.delegate = self
    }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        showError(error)
    }

    func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
        showError(error)
    }

    /// iOS kills the web content process of a backgrounded app now and then;
    /// reload instead of showing a blank page.
    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
        webView.load(URLRequest(url: webView.url ?? home))
    }
}

// MARK: - Downloads (links with `download`, files the web view can't show)

extension HubViewController: WKDownloadDelegate {
    func download(_ download: WKDownload, decideDestinationUsing response: URLResponse,
                  suggestedFilename: String, completionHandler: @escaping (URL?) -> Void) {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let name = suggestedFilename.isEmpty ? "download" : suggestedFilename
        let dest = dir.appendingPathComponent(name)
        downloads[download] = dest
        completionHandler(dest)
    }

    func downloadDidFinish(_ download: WKDownload) {
        guard let file = downloads.removeValue(forKey: download) else { return }
        share(fileAt: file)
    }

    func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
        downloads.removeValue(forKey: download)
        showError(error)
    }
}

// MARK: - New windows, dialogs, camera/microphone

extension HubViewController: WKUIDelegate {
    /// `target="_blank"` / window.open: hub pages (attachments) open in a sheet
    /// that shares the login; other sites in Safari.
    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                 for action: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        guard let url = action.request.url else { return nil }
        if !isHub(url) {
            openOutside(url)
            return nil
        }
        let sheet = SheetViewController(configuration: configuration, owner: self)
        let nav = UINavigationController(rootViewController: sheet)
        nav.overrideUserInterfaceStyle = .dark
        topPresenter.present(nav, animated: true)
        return sheet.webView
    }

    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
        let alert = UIAlertController(title: nil, message: message, preferredStyle: .alert)
        alert.addAction(UIAlertAction(title: "OK", style: .default) { _ in completionHandler() })
        topPresenter.present(alert, animated: true)
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let alert = UIAlertController(title: nil, message: message, preferredStyle: .alert)
        alert.addAction(UIAlertAction(title: "Cancel", style: .cancel) { _ in completionHandler(false) })
        alert.addAction(UIAlertAction(title: "OK", style: .default) { _ in completionHandler(true) })
        topPresenter.present(alert, animated: true)
    }

    func webView(_ webView: WKWebView, runJavaScriptTextInputPanelWithPrompt prompt: String,
                 defaultText: String?, initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping (String?) -> Void) {
        let alert = UIAlertController(title: nil, message: prompt, preferredStyle: .alert)
        alert.addTextField { $0.text = defaultText }
        alert.addAction(UIAlertAction(title: "Cancel", style: .cancel) { _ in completionHandler(nil) })
        alert.addAction(UIAlertAction(title: "OK", style: .default) { _ in
            completionHandler(alert.textFields?.first?.text)
        })
        topPresenter.present(alert, animated: true)
    }

    func webView(_ webView: WKWebView, requestMediaCapturePermissionFor origin: WKSecurityOrigin,
                 initiatedByFrame frame: WKFrameInfo, type: WKMediaCaptureType,
                 decisionHandler: @escaping (WKPermissionDecision) -> Void) {
        // The hub only; iOS still asks the user once per app.
        decisionHandler(origin.host.lowercased() == home.host?.lowercased() ? .grant : .prompt)
    }
}

/// A hub page opened as a new window (an attachment): shown in a sheet with
/// Done and Share (Share downloads the file with the same login).
final class SheetViewController: UIViewController, WKNavigationDelegate, WKUIDelegate {
    let webView: WKWebView
    private weak var owner: HubViewController?

    init(configuration: WKWebViewConfiguration, owner: HubViewController) {
        webView = WKWebView(frame: .zero, configuration: configuration)
        self.owner = owner
        super.init(nibName: nil, bundle: nil)
    }

    required init?(coder: NSCoder) { fatalError("not used") }

    override func loadView() {
        webView.navigationDelegate = self
        webView.uiDelegate = self
        webView.backgroundColor = UIColor(Color.hubBackground)
        webView.isOpaque = false
        view = webView
    }

    override func viewDidLoad() {
        super.viewDidLoad()
        navigationItem.leftBarButtonItem = UIBarButtonItem(systemItem: .done, primaryAction: UIAction { [weak self] _ in
            self?.dismiss(animated: true)
        })
        navigationItem.rightBarButtonItem = UIBarButtonItem(systemItem: .action, primaryAction: UIAction { [weak self] _ in
            guard let self, let url = self.webView.url else { return }
            self.webView.startDownload(using: URLRequest(url: url)) { download in
                download.delegate = self.owner
            }
        })
    }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        title = webView.title
    }

    func webView(_ webView: WKWebView, decidePolicyFor response: WKNavigationResponse,
                 decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
        decisionHandler(response.canShowMIMEType ? .allow : .download)
    }

    func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
        download.delegate = owner
    }

    func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
        download.delegate = owner
    }

    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        decisionHandler(action.shouldPerformDownload ? .download : .allow)
    }
}
