<!-- source: README.md; sha256: 008297f255e89d26769a8517dd1c425b15462564563cce18859b56b7640a570e -->
# Crabbox {#crabbox}

**Computer auf Abruf für Agenten.**

## Was Crabbox ist {#what-crabbox-is}

Crabbox stellt Agenten und Menschen Computer für Tests und Builds bereit. Bearbeite lokal weiter, während eine Remote-Maschine deinen Arbeitsbaum einschließlich nicht eingecheckter Änderungen ausführt. Crabbox streamt Ausgabe, liefert den Exitcode und bereinigt das Lease gemäß Anbietervertrag.

Mit konfiguriertem Anbieter führst du den gewohnten Testbefehl aus:

```sh
crabbox run -- pnpm test
```

## Ein Ablauf, viele Maschinen {#one-loop-many-kinds-of-box}

Der übliche Remote-Ablauf:

1. **Lokal bearbeiten** in der vorhandenen Arbeitskopie.
2. **Maschine mieten** oder warme Kapazität wiederverwenden.
3. Den Arbeitsbaum samt nicht eingecheckter Änderungen **synchronisieren**.
4. Den Befehl auf der Maschine **ausführen**.
5. **Ausgabe streamen und Exitcode zurückgeben**.
6. Eigene Kapazität **bereinigen** oder für den nächsten Lauf behalten.

Delegierte Anbieter können Ausführung und Synchronisation übernehmen. Wiederverwendung, Nachweise und Bereinigung richten sich nach Anbieterfähigkeiten und Ausführungsrichtlinie.

Die Infrastruktur kann wechseln, ohne den Ablauf in ein Anbieterskript umzubauen. Beginne bei [Anwendungsfällen](use-cases.md), wenn die Aufgabe feststeht, oder bei der [Anbieterreferenz](providers/README.md), wenn das Ziel bekannt ist.

## Mehr Agenten, weniger Warten {#more-agents-less-waiting}

- **Parallele Agenten:** Tests und Builds auf getrennten Maschinen statt Konkurrenz um CPU, RAM und Ports.
- **Warme Maschinen:** Vorbereitete Umgebungen verkürzen Iterationen.
- **Plattformübergreifend:** Linux, macOS, natives Windows und WSL2 aus derselben Arbeitskopie, je nach Anbieter.
- **Sehen und steuern:** [VNC oder WebVNC](features/interactive-desktop-vnc.md), Browser oder VS Code im Browser mit `--code` auf verwaltetem Linux. Das authentifizierte [Portal](features/portal.md) zeigt Leases und Logs. WebVNC erlaubt Beobachtung und Übernahme bei laufender lokaler Bridge.
- **Gemeinsame Team-Kapazität:** Ein optionaler Koordinator verwaltet Zugangsdaten, gemeinsame Leases, Ablauf und konfigurierte Ausgabenlimits vermittelter Anbieter.

Der [Katalog](providers/README.md) umfasst Clouds, lokale VMs, SSH-Hosts und delegierte Sandboxes. Der [externe Plugin-Vertrag](providers/external.md) bindet eigene Backends an. Anbieterübersicht und Kategorienzahlen entstehen aus Katalogmetadaten.

## Hier beginnen {#start-here}

- [Erste Schritte](getting-started.md) — Installation und erster Remote-Lauf.
- [Anwendungsfälle](use-cases.md) — schnelle Rückmeldung, Agenten, OS-Prüfung, Browser-QA, Parallelisierung und GPU.
- [Preise und Kosten](pricing.md) — Open Source, eigene Rechenleistung und Schutzgrenzen.
- [So funktioniert Crabbox](how-it-works.md) — CLI, Koordinator und Runner.
- [Crabbox-Vision](../VISION.md) — Umfang, Nichtziele und sichere Lebenszyklen.

## Betriebsmodell wählen {#pick-an-operating-model}

| Weg | Geeignet für | Eigentum |
| --- | --- | --- |
| Lokale Laufzeit | Schnelle Entwicklung ohne Zugangsdaten | Arbeitsplatz und lokale Laufzeit |
| Direkte Cloud oder SSH | Persönliche Konten, private Hosts, eigene Virtualisierung | Eigenes Konto oder Infrastruktur |
| Team-Koordinator | Gemeinsame Zugangsdaten, Leases, Bereinigung, Nutzung und Limits | Eigenes Cloudflare oder Node.js/PostgreSQL |
| Delegierte Ausführung | Anbieterbestimmte Agenten-, CI-, Browser- oder GPU-Läufe | Anbieter oder Laufzeitbetreiber |

Crabbox ist MIT-lizenziert. Es gibt derzeit keinen gehosteten Steuerungstarif; Rechenleistung und Koordinator-Infrastruktur werden von ihren Betreibern abgerechnet. Siehe [Preise und Kosten](pricing.md).

## Vertrauensgrenze {#trust-boundary}

Nutze vertrauenswürdige Repositories und Teammitglieder. Isolation hängt von der Laufzeit ab. Wiederverwendung und destruktive Bereinigung benötigen bestätigtes Eigentum am genauen Objekt und Claim. Unklarheit führt zur Ablehnung.

Wähle mit [Anbieterauswahl](features/provider-selection.md) und lies Anbieterunterlagen sowie [Sicherheitsmodell](security.md). Empfehlungen sind Ablaufhilfen, keine Sicherheitszertifizierung.

## Weiterführend {#go-deeper}

- **CLI und Konfiguration:** [CLI](cli.md), [Befehle](commands/README.md), [Konfiguration](features/configuration.md), [Repository-Einrichtung](features/repository-onboarding.md).
- **Flotte und Betrieb:** [Architektur](architecture.md), [Infrastruktur](infrastructure.md), [Betrieb](operations.md), [Beobachtbarkeit](observability.md), [Nur lesende Gerätekopplung](features/device-pairing.md), [Sicherheit](security.md).
- **Läufe und Nachweise:** [Jobs](features/jobs.md), [Actions-Vorbereitung](features/actions-hydration.md), [Artefakte](features/artifacts.md), [Checkpoints](features/checkpoints.md), [Interaktiver Desktop und VNC](features/interactive-desktop-vnc.md).
- **Plattformgrenzen:** [Verschachtelte Ausführung](features/nested-execution.md) unterscheidet WSL2, Container, vorbereitete KVM-Hosts und lokale Sandboxes.
- **Erweiterung:** [Integrationen](integrations/README.md), [Anbieterentwicklung](features/provider-authoring.md), [Quellcodekarte](source-map.md).

## Über diese Dokumentation {#about-these-docs}

Das Markdown in `docs/` ist die Quelle von [crabbox.sh](https://crabbox.sh/). Der Code ist maßgeblich; die [Quellcodekarte](source-map.md) ordnet Verhalten seiner Implementierung zu.

Website lokal bauen und prüfen:

```sh
scripts/check-docs.sh
open dist/docs-site/index.html
```

Verwende in `docs/` Überschriften der Ebenen 1–4 und Codeblöcke mit drei Backticks. Überschriftenlinks werden gegen dieselben IDs geprüft, die der Renderer erzeugt; Beispiele und Kommentare reservieren keine Anker. Erhalte veröffentlichte IDs. Reines Repository-Markdown nutzt weiterhin eigene GitHub-orientierte Ankerregeln, ohne vollständige GitHub-Markdown-Unterstützung zu behaupten.

Siehe [Dokumentation übersetzen](translations.md), um eine Sprache hinzuzufügen oder zu aktualisieren.
