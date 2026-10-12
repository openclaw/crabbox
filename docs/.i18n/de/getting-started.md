<!-- source: getting-started.md; sha256: 6f010425dbbffe15196dfbc9cbfce66ea14783090c7babf231211bf13239fc09 -->
# Erste Schritte {#getting-started}

Crabbox gibt Agenten und Menschen Computer auf Abruf. Bearbeite lokal weiter: Crabbox mietet eine Maschine, synchronisiert nicht eingecheckte Änderungen, führt Befehle aus und liefert Ausgabe und Exitcode. Einmalige Läufe geben sie frei; warme Leases bleiben verfügbar.

Remote-Tests und Builds entlasten CPU, Speicher und Ports für parallele Agenten. Wähle einen Anbieter für Linux, macOS, natives Windows oder WSL2. Sichtbare Arbeit erklärt der [Desktop-, VNC-, Browser-, Code- und Portal-Leitfaden](features/interactive-desktop-vnc.md).

Diese Anleitung führt ein vertrauenswürdiges Repository bis zu `crabbox run -- pnpm test`. Prüfe Konfiguration und Einrichtung vor Ausführung. Ohne Konto nutzt `--provider local-container` lokales Docker oder Podman; der [`crabbox-quickstart`-Skill](integrations/agents.md#install-through-ecosystem-skill-managers) erklärt den gesamten zugangsdatenfreien Weg.

## Schritt 1. Installieren {#step-1-install}

```sh
brew install openclaw/tap/crabbox
```

Installation prüfen:

```sh
crabbox --version
crabbox doctor
```

`crabbox doctor` schreibt je Prüfung eine Zeile. Frisch installiert sollte der neutrale `git`-Check `ok` melden; Anbieterprüfungen beginnen nach Auswahl. Ohne Auswahl meldet er `no provider selected`, behält `source=compiled_default selected=false` und überspringt Zugangsdatenprüfungen ohne Fehler. Wähle vor Lebenszyklusbefehlen über Konfiguration, `CRABBOX_PROVIDER` oder `--provider`. Danach wird streng geprüft; fehlende Zugangsdaten können scheitern. Vergleiche mit `crabbox providers recommend`.

Ohne Homebrew gibt es Archive für macOS, Linux und Windows unter <https://github.com/openclaw/crabbox/releases>. macOS-Dateien sind signiert und notarisiert.

Belasse `crabbox-runtime` neben der eigentlichen CLI. Beide Linux-Architekturen gehören zum passenden Controller. Nur die CLI zu kopieren oder Versionen zu mischen erhält nicht das vollständige Paket.

Go-Nutzer können nur die CLI einer konkreten Version installieren. Wähle ein Tag aus [GitHub Releases](https://github.com/openclaw/crabbox/releases); unterstützt ab v0.44.0:

```sh
go install github.com/openclaw/crabbox/cmd/crabbox@v0.70.0
```

Das Modul benötigt Go 1.26 und bevorzugt go1.26.5. Nutze Go 1.26.5 oder neuer oder automatische Toolchain-Auswahl. Dabei wird nur die CLI kompiliert, nicht `crabbox-apple-vm-helper`, Runtime-Paket oder andere Begleitdateien installiert. Es ist keine signierte/notarisierte Binärdistribution. Nutze Homebrew oder Archive für das vollständige Paket, besonders Apple VM auf Apple Silicon.

WSL2- und x64-Windows-Transfer ohne WSL benötigen aktuelles `main` oder Crabbox ab v0.42.1. Siehe [Windows-Installation](windows-install.md).

## Schritt 2. Anmelden {#step-2-log-in}

Für gemeinsame Flotten nutze die Team-Koordinator-URL. Sie verwaltet Zugangsdaten, Lease-Limits, Ausgaben und Ablauf. Direkte und lokale Anbieter benötigen kein Login; siehe [Zugriffsweg wählen](#choosing-an-access-path).

```sh
crabbox login --url https://broker.example.com
```

`login` öffnet GitHub OAuth; `--no-browser` zeigt die URL für einen Browser desselben Geräts. Der Broker tauscht den Code, prüft Organisationsmitgliedschaft und leitet eine einmalige Bestätigung zum Loopback-Listener der CLI zurück. Danach speichert er das signierte Token:

```text
logged in broker=https://broker.example.com provider=hetzner user=github:12345 org=example-org config=/Users/alice/Library/Application Support/crabbox/config.yaml
```

Weitere `crabbox`-Befehle authentifizieren automatisch. Identität prüfen:

```sh
crabbox whoami
```

```text
user=github:12345 org=example-org auth=user broker=https://broker.example.com
```

### Zugriffsweg wählen {#choosing-an-access-path}

Broker-Zugriff hängt vom Deployment ab. Nutze die Team-URL und GitHub-Organisations-/Team-Freigabeliste. Auch erfolgreiches OAuth kann außerhalb der Liste abgelehnt werden.

Für persönliche oder fremde Installationen:

- **Direkter Anbieter:** lokale Cloud-Zugangsdaten für private Tests; lokale Bereinigung und Zustand statt zentraler Historie und gemeinsamer Limits.
- **Eigener Koordinator:** Cloudflare Workers oder Node.js/PostgreSQL für zentrale Zugangsdaten, aktive Lease-Limits, monatliche Budgets, `crabbox usage`, dauerhafte Bereinigung und Team-Endpunkt.
- **Zugriff beantragen:** nur bei definiertem Aufnahmeweg für deine Organisation. Team-Endpunkte sind nicht automatisch öffentlich.

Direkter Modus überspringt `login`:

```sh
crabbox doctor --provider hetzner
crabbox run --provider hetzner -- pnpm test
```

Wähle für Selbsthosting eine Laufzeit:

- **Cloudflare Workers:** Durable Object, Alarme, geplante Bereinigung, optional Cloudflare Access.
- **Node.js/PostgreSQL:** HTTP/WebSocket, PostgreSQL 13+, pg-boss-Wartungsjobs, eine dauerhafte Replik. Als anfängliche portable Laufzeit betrachten und vor Produktionsumstellung Deployment-Nachweis abschließen.

Beide teilen Geheimnisse, Authentifizierung, Budgets, API und Portal. Siehe [Infrastruktur](infrastructure.md#choose-a-coordinator-runtime). Browser-Login benötigt OAuth-App und mindestens eine erlaubte Organisation/ein Team; Token-Automatisierung nicht.

CI ohne Browser verwendet gemeinsame Tokens:

```sh
printf '%s' "$TOKEN" | crabbox login \
  --url https://broker.example.com \
  --provider aws \
  --token-stdin
```

Das Identitätsmodell steht unter [Authentifizierung und Administration](features/auth-admin.md).

## Schritt 3. Repository einrichten {#step-3-onboard-a-repo}

Im Repository:

```sh
crabbox init
```

`init` schreibt drei Dateien. Pfade über `--config`, `--workflow` oder ein/mehrere `--skill` ändern, bestehende Dateien mit `--force` überschreiben:

```text
.crabbox.yaml                          repo defaults (profile, class, sync, env)
.github/workflows/crabbox.yml          Actions hydration workflow (optional)
.agents/skills/crabbox/SKILL.md        agent-facing skill instructions
```

Passe `.crabbox.yaml` an:

- `profile`: Name der Testspur; Vorlage `<repo>-check`.
- `class`: `tiny`, `small`, `standard`, `fast`, `large`, `beast`; Vorlage `beast`.
- `sync.exclude`: nie zu sendende Verzeichnisse.
- `sync.include`: optionale rootrelative Freigabeliste; nach Ausschlüssen werden **nur** diese Pfade übertragen.
- `env.allow`: für Remote-Befehle sichtbare Umgebungsvariablen.

`--detect` erkennt Testbefehle und schreibt `jobs.detected`, ausführbar mit `crabbox job run detected`.

Übertragung prüfen:

```sh
crabbox sync-plan
```

`sync-plan` zeigt Dateianzahl, Bytes und größte Dateien. Bei unerwartetem `dist/`, `.cache/` oder großen Assets `sync.exclude` enger fassen. Diese Größe bestimmt die erste Synchronisation.

Native Git-backed Jujutsu-Arbeitsbereiche funktionieren mit installiertem `jj`. Crabbox erfasst Änderungen und synchronisiert den genauen `@`-Baum einschließlich Mehr-Eltern-Merges. `sync-plan --json` zeigt die Revision; bei normalen SSH-Anbietern `run --no-hydrate` verwenden. Zusammenliegende Git/Jujutsu-Bereiche nutzen Git. Voraussetzungen und sichere Ablehnung: [Jujutsu-Sync](features/sync.md#jujutsu-workspaces).

## Schritt 4. Maschine vorbereiten {#step-4-warm-a-box}

```sh
crabbox warmup --class standard --slug first-tests
```

`warmup` mietet, provisioniert, wartet auf SSH/Werkzeuge und behält das Lease (`--keep` standardmäßig an). Es zeigt Übersicht, Endpunkt und Dauer:

```text
leased cbx_abcdef123456 slug=first-tests provider=hetzner server=... type=... ip=203.0.113.10 idle_timeout=30m0s expires=...
ready ssh=crabbox@203.0.113.10:2222 network=public workroot=/work/crabbox
warmup complete total=...
```

Der Koordinator hält sie bereit. Standardmäßig begrenzen 30 Minuten Leerlauf und 90 Minuten TTL die Lebensdauer. Direkte Anbieter haben eigene Ablaufregeln; nach Gebrauch immer stoppen. Klassen sind anbieterspezifisch: vorher `crabbox providers --json` prüfen.

Über `slug` oder `id` (`cbx_...`) wiederverwenden; beide gelten für `--id`.

## Schritt 5. Befehl ausführen {#step-5-run-a-command}

```sh
crabbox run --id first-tests -- pnpm test
```

Ablauf:

1. SSH-Bereitschaft prüfen.
2. Remote-Git aus Origin/Basisreferenz vorbereiten, lokale Änderungen per rsync darüberlegen; gleiche Fingerprints überspringen den Sync.
3. Per SSH ausführen und stdout/stderr streamen.
4. Heartbeats verhindern Leerlaufablauf während des Laufs.
5. `run_...` mit Sync-/Befehlsdauer, Exitcode und auf Linux begrenzter Telemetrie speichern.

Werkzeuge und Abhängigkeiten bleiben Repository-Aufgabe. Führe Einrichtung oder [Actions-Hydrierung](features/actions-hydration.md) vorher aus.

Für einen Einmallauf `--id` weglassen:

```sh
crabbox run -- pnpm test
```

Ein neues Lease wird danach freigegeben. Für Iterationen auf derselben Maschine `warmup` + `--id` verwenden.

## Schritt 6. Verlauf ansehen {#step-6-inspect-history}

```sh
crabbox history
crabbox events run_abcdef123456
crabbox logs run_abcdef123456
crabbox results run_abcdef123456
```

`history` listet Läufe, `events` die geordneten Lease-/Sync-/Befehls-/Ausgabe-/Endereignisse, `logs` die gespeicherte Ausgabe, `results` angehängte JUnit-Ergebnisse.

Bei aktivem Portal zeigt `/portal/runs/run_abcdef123456` dieselben Daten.

## Schritt 7. Lease stoppen {#step-7-stop-the-lease}

Nach Abschluss:

```sh
crabbox stop first-tests
```

`stop` bereinigt nach Anbietervertrag und gibt reservierte Kosten frei. Wegwerfmaschinen werden meist gelöscht; statische SSH-Hosts und bewusst erhaltene Ressourcen folgen ihren Regeln. Eigentum muss verifiziert sein. Wiederherstellungs-Claims oder terminale Quittungen fester IDs können lokal bleiben. Vergessene vermittelte Maschinen bereinigt der Koordinator nach Ablauf.

```sh
crabbox cleanup --dry-run
```

`cleanup` entfernt direkte Überreste und lokalen Zustand. Broker-Ressourcen bereinigt dessen Alarm.

## Häufige Varianten {#common-variations}

Längere Sitzung erhalten:

```sh
crabbox warmup --idle-timeout 4h --ttl 8h --slug swift-crab
crabbox run --id swift-crab -- pnpm test
crabbox run --id swift-crab -- pnpm bench
crabbox stop swift-crab
```

Desktop öffnen:

```sh
crabbox warmup --desktop --browser --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

code-server öffnen:

```sh
crabbox warmup --code --slug code-check
crabbox run --id code-check --sync-only
crabbox code --id code-check --open
```

Halte Bridges während der Browsernutzung aktiv. Desktop/Code hängen vom Anbieter und Ziel ab; Browser-Code braucht Koordinator und isolierten Code-Origin. Der [gemeinsame Leitfaden](features/interactive-desktop-vnc.md) beschreibt VNC, WebVNC, Portal, Teilen und Bereinigung.

Synchronisierte Arbeitskopie in Zed Remote Projects öffnen:

```sh
crabbox run --id swift-crab --sync-only
crabbox open --editor=zed --id swift-crab
```

SSH-Befehl in **Connect New Server** einfügen, angezeigten Ordner öffnen und `crabbox open` während der Arbeit laufen lassen.

Vorhandenen Mac verwenden (statisches SSH, keine Provisionierung):

```yaml
# .crabbox.yaml
provider: ssh
target: macos
static:
  host: mac-studio.local
  user: alice
  port: "22"
  workRoot: /Users/alice/crabbox
```

```sh
crabbox run -- xcodebuild test
```

Anbieter und Klasse pro Befehl überschreiben:

```sh
crabbox run --provider aws --class beast -- pnpm test
```

## Weiterführend {#where-to-go-next}

- [Funktionsweise](how-it-works.md) — Grundmodell.
- [CLI](cli.md) — Befehle und Exitcodes.
- [Befehle](commands/README.md) — Einzelreferenzen.
- [Funktionen](features/README.md) — Einzelthemen.
- [Konfiguration](features/configuration.md) — YAML und Priorität.
- [Anbieter](providers/README.md) — Auswahl.
- [Anbieterentwicklung](features/provider-authoring.md) — neue Anbieter.
- [Fehlersuche](troubleshooting.md) — Probleme lösen.
