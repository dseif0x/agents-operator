import AppIntents

/// "Open Agents" in Shortcuts (and for the Action Button via a shortcut).
/// The Action Button can also pick the app directly under "App".
struct OpenAgentsIntent: AppIntent {
    static var title: LocalizedStringResource = "Open Agents"
    static var description = IntentDescription("Opens the agents-operator hub.")
    static var openAppWhenRun = true

    @MainActor
    func perform() async throws -> some IntentResult {
        .result()
    }
}

struct AgentsShortcuts: AppShortcutsProvider {
    static var appShortcuts: [AppShortcut] {
        AppShortcut(
            intent: OpenAgentsIntent(),
            phrases: ["Open \(.applicationName)"],
            shortTitle: "Open Agents",
            systemImageName: "terminal"
        )
    }
}
