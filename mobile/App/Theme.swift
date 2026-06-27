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
    static let bg        = Color(hex: 0x0A0A0B)
    static let panel     = Color(hex: 0x151517)
    static let raised    = Color(hex: 0x1E1E22)
    static let accent    = Color(hex: 0x35E0BE)
    static let accentDim = Color(hex: 0x35E0BE, opacity: 0.6)
    static let textPrimary   = Color(hex: 0xF6F7F5)
    static let textSecondary = Color(hex: 0x8A929C)
    static let danger    = Color(hex: 0xFF7A66)

    /// Standard corner radius for cards and raised controls.
    static let cornerRadius: CGFloat = 16
    /// Tighter radius for chips/pills.
    static let pillRadius: CGFloat = 10

    /// Consistent spacing scale (8-pt rhythm) used across every screen.
    enum Space {
        static let xs: CGFloat = 4
        static let sm: CGFloat = 8
        static let md: CGFloat = 12
        static let lg: CGFloat = 16
        static let xl: CGFloat = 20
        static let xxl: CGFloat = 24
    }
}

#if canImport(UIKit)
import UIKit

extension Theme {
    /// Light, premium haptic tap for buttons / toggles.
    static func haptic(_ style: UIImpactFeedbackGenerator.FeedbackStyle = .light) {
        let gen = UIImpactFeedbackGenerator(style: style)
        gen.impactOccurred()
    }
}
#endif

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
                    .strokeBorder(Color.white.opacity(0.07), lineWidth: 1)
            )
            .shadow(color: Color.black.opacity(0.22), radius: 12, x: 0, y: 5)
    }
}

extension View {
    /// Apply the standard Crabbox card chrome.
    func card(padding: CGFloat = 16) -> some View {
        modifier(CardModifier(padding: padding))
    }

    /// Authentic terminal styling: monospaced, comfortable line height, a near-black
    /// surface with a faint accent edge — a focused code-editor look.
    func terminalSurface() -> some View {
        self
            .background(Color(hex: 0x0C0C0E), in: RoundedRectangle(cornerRadius: 12, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: 12, style: .continuous)
                    .strokeBorder(Theme.accent.opacity(0.10), lineWidth: 1)
            )
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
            .padding(.vertical, 15)
            .padding(.horizontal, fullWidth ? 0 : 20)
            .background(Theme.accent.opacity(configuration.isPressed ? 0.82 : 1))
            .clipShape(RoundedRectangle(cornerRadius: 13, style: .continuous))
            .shadow(color: Theme.accent.opacity(configuration.isPressed ? 0.10 : 0.28), radius: 10, x: 0, y: 4)
            .scaleEffect(configuration.isPressed ? 0.98 : 1)
            .animation(.spring(response: 0.3, dampingFraction: 0.7), value: configuration.isPressed)
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
            .padding(.vertical, 15)
            .padding(.horizontal, fullWidth ? 0 : 20)
            .background(Theme.raised.opacity(configuration.isPressed ? 0.7 : 1))
            .clipShape(RoundedRectangle(cornerRadius: 13, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: 13, style: .continuous)
                    .strokeBorder(Color.white.opacity(0.08), lineWidth: 1)
            )
            .scaleEffect(configuration.isPressed ? 0.98 : 1)
            .animation(.spring(response: 0.3, dampingFraction: 0.7), value: configuration.isPressed)
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
            .shadow(color: Color.black.opacity(0.22), radius: 12, x: 0, y: 5)
    }
}
