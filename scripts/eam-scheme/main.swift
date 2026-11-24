// eam:// for macOS.
//
// A web page cannot claim an operating-system URL scheme: only a bundle that declares
// CFBundleURLTypes can. This helper is that bundle, and it does one thing — turn
// eam:///files/<path> into https://everythingabout.me/files/<path> and hand it to the
// default browser. It is a local convenience for this machine, not a service feature: a
// link sent to someone else opens nothing unless they installed it too.
import Cocoa

/// Where every eam:// link lands. The scheme is indirection: if the drive ever moves, only
/// this line and the app bundle's plist change, not the links already pasted into notes.
let base = URL(string: "https://everythingabout.me/")!

final class Delegate: NSObject, NSApplicationDelegate {
    private var handled = false

    func applicationDidFinishLaunching(_ notification: Notification) {
        // Launched by hand (or by a mis-registered handler) there is no URL to forward, and
        // an invisible process must not linger. URL deliveries for a fresh launch arrive
        // well inside this window.
        DispatchQueue.main.asyncAfter(deadline: .now() + 3) {
            if !self.handled { NSApp.terminate(nil) }
        }
    }

    func application(_ application: NSApplication, open urls: [URL]) {
        handled = true
        for url in urls { forward(url) }
        NSApp.terminate(nil)
    }

    private func forward(_ url: URL) {
        guard url.scheme?.lowercased() == "eam" else { return }
        // eam:///files/Photos/a.jpg -> https://everythingabout.me/files/Photos/a.jpg
        // The address is the path below /files, exactly as in the web address, so there is
        // no host part to read. eam:/files/... and eam://files/... are accepted too, because
        // shells and editors both produce them.
        var rest = url.absoluteString.dropFirst("eam://".count)
        while rest.hasPrefix("/") { rest = rest.dropFirst() }
        guard !rest.isEmpty, let target = URL(string: String(rest), relativeTo: base) else { return }
        NSWorkspace.shared.open(target)
    }
}

let app = NSApplication.shared
let delegate = Delegate()
app.delegate = delegate
// No dock icon and no window: this is a link handler, not an app you switch to.
app.setActivationPolicy(.accessory)
app.run()
