import SwiftUI

/// The Crabbox design language: a dark, high-contrast surface with a single
/// minty accent. Colors are centralized here so every screen stays consistent
/// and a future re-theme touches exactly one file.
///
/// Palette (hex → use):
///   #101010  app background
///   #171717  panels / cards
///   #202020  raised controls / hairlines
///   #31d0aa  accent (primary actions, "Live" pill, progress)
///   #f7f7f4  primary text
///   #9fa6ad  secondary text
///   #ff7966  destructive / "Offline"
enum Theme {
    static let bg        = Color(hex: 0x101010)
    static let panel     = Color(hex: 0x171717)
    static let raised    = Color(hex: 0x202020)
    static let accent    = Color(hex: 0x31D0AA)
    static let textPrimary   = Color(hex: 0xF7F7F4)
    static let textSecondary = Color(hex: 0x9FA6AD)
    static let danger    = Color(hex: 0xFF7966)

    /// Standard corner radius for cards and raised controls.
    static let cornerRadius: CGFloat = 16
    /// Tighter radius for chips/pills.
    static let pillRadius: CGFloat = 10
}

extension Color {
    /// Build a `Color` from a 24-bit RGB hex literal (e.g. `0x31D0AA`).
    init(hex: UInt32, opacity: Double = 1) {
        let r = Double((hex >> 16) & 0xFF) / 255
        let g = Double((hex >> 8) & 0xFF) / 255
        let b = Double(hex & 0xFF) / 255
        self.init(.sRGB, red: r, green: g, blue: b, opacity: opacity)
    }
}

// MARK: - Reusable styling

/// Wraps content in the standard Crabbox card: a `#171717` panel with a soft
/// hairline border and rounded corners.
struct CardModifier: ViewModifier {
    var padding: CGFloat = 16
    func body(content: Content) -> some View {
        content
            .padding(padding)
            .background(Theme.panel)
            .clipShape(RoundedRectangle(cornerRadius: Theme.cornerRadius, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: Theme.cornerRadius, style: .continuous)
                    .strokeBorder(Color.white.opacity(0.06), lineWidth: 1)
            )
    }
}

extension View {
    /// Apply the standard Crabbox card chrome.
    func card(padding: CGFloat = 16) -> some View {
        modifier(CardModifier(padding: padding))
    }
}

/// A small status/label pill. Used for the Portal status indicator
/// ("Live"/"Loading"/"Offline") and as a generic tag elsewhere.
struct Pill: View {
    let text: String
    var color: Color = Theme.accent
    /// When true the pill is filled with a translucent wash of `color`;
    /// otherwise it's a subtle neutral chip with colored text.
    var prominent: Bool = true

    var body: some View {
        HStack(spacing: 6) {
            Circle()
                .fill(color)
                .frame(width: 6, height: 6)
            Text(text)
                .font(.system(size: 12, weight: .semibold, design: .rounded))
                .foregroundStyle(prominent ? color : Theme.textSecondary)
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 5)
        .background(
            (prominent ? color.opacity(0.12) : Theme.raised)
        )
        .clipShape(Capsule())
        .overlay(
            Capsule().strokeBorder(color.opacity(prominent ? 0.25 : 0), lineWidth: 1)
        )
    }
}

/// The primary accent button style (filled mint, dark label). Used for the
/// dominant action on a screen ("Connect", "Launch LLM sandbox", "Send").
struct AccentButtonStyle: ButtonStyle {
    var fullWidth: Bool = true
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.system(size: 16, weight: .semibold, design: .rounded))
            .foregroundStyle(Color(hex: 0x062019))
            .frame(maxWidth: fullWidth ? .infinity : nil)
            .padding(.vertical, 13)
            .padding(.horizontal, fullWidth ? 0 : 18)
            .background(Theme.accent.opacity(configuration.isPressed ? 0.78 : 1))
            .clipShape(RoundedRectangle(cornerRadius: 12, style: .continuous))
    }
}

/// A quieter secondary button: raised panel fill, primary text. Used for
/// "Use crabbox.sh", "Cancel", and similar non-destructive secondaries.
struct SecondaryButtonStyle: ButtonStyle {
    var fullWidth: Bool = true
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.system(size: 16, weight: .medium, design: .rounded))
            .foregroundStyle(Theme.textPrimary)
            .frame(maxWidth: fullWidth ? .infinity : nil)
            .padding(.vertical, 13)
            .padding(.horizontal, fullWidth ? 0 : 18)
            .background(Theme.raised.opacity(configuration.isPressed ? 0.7 : 1))
            .clipShape(RoundedRectangle(cornerRadius: 12, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: 12, style: .continuous)
                    .strokeBorder(Color.white.opacity(0.07), lineWidth: 1)
            )
    }
}

// MARK: - Aliases used across screens

/// Additional named tokens so every screen can reach the palette by an intuitive
/// name. (Kept as aliases rather than renaming to avoid churn across views.)
extension Theme {
    static let background = bg
    static let textMuted  = textSecondary
    static let field      = raised        // text-input / control fill
    static let hairline   = Color.white.opacity(0.07)
    static let divider    = Color.white.opacity(0.07)
    static let subtle     = textSecondary
}

/// The primary (accent) button style, named for screens that prefer it.
typealias PrimaryButtonStyle = AccentButtonStyle

/// A standard Crabbox content card as a container view (panel fill + hairline +
/// rounded corners). Mirrors the `.card()` modifier for call sites that prefer a
/// wrapping view: `Card { … }`.
struct Card<Content: View>: View {
    private let content: Content
    init(@ViewBuilder content: () -> Content) { self.content = content() }
    var body: some View {
        VStack(alignment: .leading, spacing: 0) { content }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(16)
            .background(Theme.panel)
            .clipShape(RoundedRectangle(cornerRadius: Theme.cornerRadius, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: Theme.cornerRadius, style: .continuous)
                    .strokeBorder(Theme.hairline, lineWidth: 1)
            )
    }
}
