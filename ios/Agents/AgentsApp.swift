import SwiftUI

/// A full-screen wrapper around the agents-operator web app, so it is a real
/// app: it shows up for the Action Button, in Shortcuts ("Open App") and in
/// Spotlight, which a Home Screen web app does not.
@main
struct AgentsApp: App {
    /// The hub address. Set on first launch, changeable in iOS Settings ->
    /// Agents (Settings.bundle writes the same key).
    @AppStorage("hubURL") private var hubURL = ""

    var body: some Scene {
        WindowGroup {
            ZStack {
                Color.hubBackground.ignoresSafeArea()
                if let url = HubURL.parse(hubURL) {
                    HubView(url: url)
                        .ignoresSafeArea()
                        .id(url) // a changed address loads a fresh web view
                } else {
                    SetupView(hubURL: $hubURL)
                }
            }
            .preferredColorScheme(.dark)
        }
    }
}

extension Color {
    /// The web app's theme/background colour (#0f1115).
    static let hubBackground = Color(red: 0x0F / 255, green: 0x11 / 255, blue: 0x15 / 255)
}

enum HubURL {
    /// Accepts "agents.example.com" or a full URL; https unless http is given.
    static func parse(_ raw: String) -> URL? {
        var s = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !s.isEmpty else { return nil }
        if !s.lowercased().hasPrefix("http://") && !s.lowercased().hasPrefix("https://") {
            s = "https://" + s
        }
        guard let url = URL(string: s), let host = url.host, !host.isEmpty else { return nil }
        return url
    }
}

/// First launch: ask for the hub address.
struct SetupView: View {
    @Binding var hubURL: String
    @State private var draft = ""
    @FocusState private var focused: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("agents-operator")
                .font(.largeTitle.bold())
            Text("Enter the address of your hub. You can change it later in Settings → Agents.")
                .foregroundStyle(.secondary)
            TextField("https://agents.example.com", text: $draft)
                .keyboardType(.URL)
                .textContentType(.URL)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .focused($focused)
                .padding(12)
                .background(Color.white.opacity(0.08), in: RoundedRectangle(cornerRadius: 10))
                .onSubmit(save)
            Button(action: save) {
                Text("Continue").frame(maxWidth: .infinity).padding(.vertical, 6)
            }
            .buttonStyle(.borderedProminent)
            .disabled(HubURL.parse(draft) == nil)
            Spacer()
        }
        .padding(24)
        .onAppear { focused = true }
    }

    private func save() {
        guard let url = HubURL.parse(draft) else { return }
        hubURL = url.absoluteString
    }
}
