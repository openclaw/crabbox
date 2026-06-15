import CrabboxKit
import Foundation

/// A named end-to-end scenario: a sequence of actions plus scenario-specific
/// assertions. Invariants 1–13 are checked automatically after every dispatch.
public struct Scenario {
    public let name: String
    public let env: AppEnv
    public let body: (Sim) -> Void
}

private let prod = AppEnv(allowLocalHTTP: false)
private let dev = AppEnv(allowLocalHTTP: true)

public let scenarios: [Scenario] = [
    Scenario(name: "boot_empty_storage", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        sim.loadCycle(url: defaultCoordinatorURL, title: "Crabbox")
        sim.expect(sim.state.homeURL == defaultCoordinatorURL, "home should be default")
        sim.expect(sim.state.persistedCoordinator == nil, "nothing persisted on empty boot")
        sim.expect(!sim.state.booting && !sim.state.isLoading, "boot and load complete")
    },

    Scenario(name: "boot_stored_https", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: "https://broker.example.com/x?q=1#h"))
        sim.loadCycle(url: "https://broker.example.com/x", title: "Broker")
        sim.expect(sim.state.homeURL == "https://broker.example.com/x", "stored https normalized")
        sim.expect(sim.state.persistedCoordinator == "https://broker.example.com/x", "persisted mirror set")
    },

    Scenario(name: "boot_stored_invalid_falls_back", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: "http://broker.example.com"))
        sim.expect(sim.state.homeURL == defaultCoordinatorURL, "invalid stored falls back to default")
        sim.expect(sim.state.persistedCoordinator == nil, "invalid stored not mirrored")
    },

    Scenario(name: "switch_to_valid_https", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        sim.dispatch(.openSettings)
        sim.dispatch(.setDraft(url: "crabbox.sh/team?token=x#s"))
        sim.dispatch(.saveCoordinator)
        sim.loadCycle(url: "https://crabbox.sh/team", title: "Team")
        sim.expect(sim.state.homeURL == "https://crabbox.sh/team", "switched to normalized https")
        sim.expect(!sim.state.settingsVisible, "settings closed after save")
        sim.expect(sim.state.persistedCoordinator == "https://crabbox.sh/team", "persisted equals home")
    },

    Scenario(name: "reject_http_prod", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        sim.dispatch(.openSettings)
        sim.dispatch(.setDraft(url: "http://broker.example.com"))
        sim.dispatch(.saveCoordinator)
        sim.expect(!sim.state.urlError.isEmpty, "http rejected with error")
        sim.expect(sim.state.settingsVisible, "settings stay open on failure")
        sim.expect(sim.state.homeURL == defaultCoordinatorURL, "home unchanged on failure")
    },

    Scenario(name: "accept_http_localhost_dev", env: dev) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        sim.dispatch(.openSettings)
        sim.dispatch(.setDraft(url: "http://localhost:8787"))
        sim.dispatch(.saveCoordinator)
        sim.loadCycle(url: "http://localhost:8787/run", title: "Local")
        sim.expect(sim.state.homeURL == "http://localhost:8787", "loopback http accepted in dev")
        sim.expect(webViewOriginWhitelist(sim.state.homeURL).contains("http://localhost:8787"), "origin whitelisted")
        sim.expect(sim.state.persistedCoordinator == "http://localhost:8787", "persisted equals home")
    },

    Scenario(name: "reject_http_localhost_prod", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        sim.dispatch(.openSettings)
        sim.dispatch(.setDraft(url: "http://localhost:8787"))
        sim.dispatch(.saveCoordinator)
        sim.expect(!sim.state.urlError.isEmpty, "loopback http rejected outside dev")
        sim.expect(sim.state.homeURL == defaultCoordinatorURL, "home unchanged")
    },

    Scenario(name: "reject_lan_http_dev", env: dev) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        sim.dispatch(.openSettings)
        sim.dispatch(.setDraft(url: "http://192.168.1.50:8787"))
        sim.dispatch(.saveCoordinator)
        sim.expect(!sim.state.urlError.isEmpty, "LAN http rejected even in dev")
        sim.expect(sim.state.persistedCoordinator == nil, "nothing persisted")
    },

    Scenario(name: "external_link_tap_mailto", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        let before = sim.state.currentURL
        let r = sim.dispatch(.shouldStartLoad(url: "mailto:team@crabbox.sh"))
        sim.expect(r.loadDecision == .external, "mailto routed external")
        sim.expect(r.effects == [.openExternal(url: "mailto:team@crabbox.sh")], "one openExternal effect")
        sim.expect(sim.state.currentURL == before, "current URL unchanged by external tap")
    },

    Scenario(name: "internal_link_loads", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        let r = sim.dispatch(.shouldStartLoad(url: "https://crabbox.sh/settings"))
        sim.expect(r.loadDecision == .load, "internal https loads")
        sim.expect(r.effects.isEmpty, "no external effect for internal nav")
        sim.dispatch(.webViewNavState(url: "https://crabbox.sh/settings", title: "Settings", canGoBack: true))
        sim.expect(sim.state.canGoBack, "canGoBack true after navigating deeper")
    },

    Scenario(name: "back_navigation", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        sim.dispatch(.webViewNavState(url: "https://crabbox.sh/settings", title: "Settings", canGoBack: true))
        let r = sim.dispatch(.goBack)
        sim.expect(r.effects == [.webViewGoBack], "goBack effect emitted")
        sim.dispatch(.webViewNavState(url: "https://crabbox.sh", title: "Crabbox", canGoBack: false))
        sim.expect(!sim.state.canGoBack, "canGoBack false at root")
    },

    Scenario(name: "back_when_blocked", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        let r = sim.dispatch(.goBack)
        sim.expect(r.effects.isEmpty, "no goBack effect when blocked")
    },

    Scenario(name: "error_then_retry", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        sim.dispatch(.webViewLoadStart)
        sim.dispatch(.webViewError)
        sim.expect(sim.state.loadFailed && !sim.state.isLoading, "offline after error")
        sim.expect(selectStatusLabel(sim.state) == .offline, "status offline")
        let r = sim.dispatch(.reload)
        sim.expect(r.effects == [.webViewReload], "reload effect emitted")
        sim.expect(!sim.state.loadFailed && sim.state.isLoading, "loading after retry")
        sim.dispatch(.webViewLoadStart)
        sim.dispatch(.webViewLoadEnd)
    },

    Scenario(name: "reset_to_default", env: dev) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        sim.dispatch(.openSettings)
        sim.dispatch(.setDraft(url: "http://localhost:8787"))
        sim.dispatch(.saveCoordinator)
        sim.dispatch(.openSettings)
        sim.dispatch(.resetCoordinator)
        sim.expect(sim.state.homeURL == defaultCoordinatorURL, "home reset")
        sim.expect(sim.state.persistedCoordinator == defaultCoordinatorURL, "persisted reset")
        sim.expect(!sim.state.settingsVisible, "settings closed after reset")
    },

    Scenario(name: "malformed_url", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        sim.dispatch(.openSettings)
        sim.dispatch(.setDraft(url: ":::: not a url"))
        sim.dispatch(.saveCoordinator)
        sim.expect(!sim.state.urlError.isEmpty, "malformed url rejected")
        sim.expect(sim.state.persistedCoordinator == nil, "nothing persisted for malformed")
        sim.expect(hostLabel(":::: not a url") == "crabbox.sh", "hostLabel falls back")
    },

    Scenario(name: "go_home_after_drift", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        sim.dispatch(.webViewNavState(url: "https://crabbox.sh/deep/page", title: "Deep", canGoBack: true))
        sim.dispatch(.goHome)
        sim.expect(sim.state.currentURL == sim.state.homeURL, "go home resets current to home")
        sim.expect(!sim.state.loadFailed, "go home clears failure")
    },

    Scenario(name: "draft_edit_clears_error", env: prod) { sim in
        sim.dispatch(.bootLoaded(storedURL: nil))
        sim.dispatch(.openSettings)
        sim.dispatch(.setDraft(url: "http://broker.example.com"))
        sim.dispatch(.saveCoordinator)
        sim.expect(!sim.state.urlError.isEmpty, "error present after bad save")
        sim.dispatch(.setDraft(url: "https://ok.example.com"))
        sim.expect(sim.state.urlError.isEmpty, "editing draft clears error")
        sim.expect(sim.state.settingsVisible, "settings still open")
    },
]
