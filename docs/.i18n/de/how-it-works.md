<!-- source: how-it-works.md; sha256: 2e0288463ff74a878d06a6226e5924a2ce5426c54796c463626ae14219ab37b5 -->
# So funktioniert Crabbox {#how-crabbox-works}

Für den Einstieg, das Gesamtmodell und die Zuordnung von Verantwortung vor Codeänderungen.

## Überblick {#tl-dr}

Crabbox bietet Computer auf Abruf. Bearbeite lokal, während Tests und Builds auf Linux, macOS, nativem Windows oder WSL2 laufen. In der Team-Flotte arbeiten zusammen:

- **CLI** (`cmd/crabbox`, `internal/cli`): mieten, Änderungen synchronisieren, ausführen, streamen, bereinigen.
- **Koordinator** (`worker/src`): Zugangsdaten, Lease-Zustand, Ablauf, Bereinigung, Nutzung und Kosten. Läuft mit Cloudflare Workers/Durable Object oder Node.js/PostgreSQL/pg-boss.

Runner halten keine Broker-Geheimnisse. Einrichtung, Abhängigkeiten und Aufgabe gehören zum Repository. Der Koordinator behält Aufzeichnungen, Logs und Telemetrie; Screenshots und Artefakte vor Freigabe herunterladen oder veröffentlichen. Direkte Anbieter kommen ohne Koordinator aus.

## Komponenten {#the-pieces}

```text
┌──────────────────────┐   HTTPS / JSON   ┌───────────────────────────┐
│ your machine         │ ───────────────► │ coordinator runtime       │
│ crabbox CLI          │  bearer + owner  │ Cloudflare + Durable Obj. │
│ repo checkout        │                  │ or Node.js + PostgreSQL   │
│ per-lease SSH key    │                  │ provider creds + state    │
└──────────┬───────────┘                  └────────────┬──────────────┘
           │                                         │ provider API
           │                                         ▼
           │                          ┌──────────────────────────────┐
           │ SSH (primary + fallback) │ managed cloud or sandbox     │
           └────────── rsync ────────►│ runner host                   │
                                      │ /work/crabbox/<lease>/<repo> │
                                      └──────────────────────────────┘
```

Die CLI spricht HTTPS mit dem Koordinator, dann **direkt** SSH/rsync mit dem Runner. Dieser ruft für normale Befehle den Koordinator nicht an; der Ausführungspfad bleibt einseitig. Der Koordinator verwaltet Leases statt Arbeitskopien zu übertragen oder Befehle auszuführen. Metadaten, Vorschauen, Logs und Ergebnisse lädt die CLI separat hoch; Portal-Bridges transportieren ebenfalls Daten darüber. Der [private AWS-Arbeitsbereichsdienst](features/aws-private-workspaces.md) ist eine getrennte SSM-only-API ohne SSH-Datenebene.

Lange Interaktionen nutzen optional einen authentifizierten `/v1/control`-WebSocket für Ereignisse und Heartbeats. Das verringert Polling bei hoher Latenz. HTTPS bleibt maßgeblicher Speicher und kompatible Ausweichroute für ältere Versionen.

## Verantwortung {#ownership}

| Schicht | Zuständig für |
| :--- | :--- |
| **CLI** | Konfiguration/Flags, Lease-SSH-Schlüssel, Bereitschaft, Git/rsync, Fingerprints/Plausibilität, Befehle/Streams, WebSocket mit HTTP-Fallback, Freigabe |
| **Broker** | Authentifizierung/Identität, serieller Zustand, Zugangsdaten, Erstellen/Löschen, Ablauf, Abfragen, Aufzeichnungen/Logs/Ereignisse/Telemetrie, Nutzung/Limits |
| **Provider** | Rechenressourcen; Adapter erstellen/löschen Hetzner, AWS EC2, Azure VMs, GCP usw. |
| **Runner** | Nichts Dauerhaftes: Linux mit SSH/Git/rsync/Arbeitswurzel, andere Ziele spezifisch vorbereitet, statische SSH-Hosts vorhanden; Repository richtet Projektruntime ein |

## Was `crabbox run` tut {#what-crabbox-run-does}

Der übliche vermittelte SSH-Lauf hat fünf Phasen. Direkte Anbieter verwalten den Lebenszyklus lokal, delegierte den Ausführungstransport.

**1. Planen.** Flags → Umgebung → Repository → Benutzer → Standardwerte. Temporäre ID aus `cbx_` und 12 Hex-Zeichen, SSH-Schlüssel unter `<user-config>/crabbox/testboxes/<lease>/id_ed25519` (RSA für AWS/Azure Windows).

**2. Mieten.** `POST /v1/leases` mit Klasse, Anbieter, Ziel, TTL, Leerlauf, Slug, Fähigkeiten und öffentlichem Schlüssel. Der Koordinator authentifiziert, serialisiert Änderungen, prüft aktive und monatliche Limits, holt Livepreise, reserviert maximale TTL-Kosten und provisioniert mit Regions-/Marktausweichwegen. Zurück kommen Host, SSH-Zugang, Arbeitswurzel, Ablauf, endgültige ID und Slug. Bei anderer ID benennt die CLI ihr Schlüsselverzeichnis um.

**3. Synchronisieren.** SSH und Bereitschaftsprobe abwarten, Git aus Origin/Basisreferenz vorbereiten. Gleiche Fingerprints überspringen rsync. Sonst POSIX-rsync nach `/work/crabbox/<lease>/<repo>` auf Linux oder Manifest-tar für natives Windows; danach Plausibilitätsprüfung und unterstützte Basishistorie vorbereiten.

**4. Ausführen.** Heartbeats im Hintergrund, SSH-Ausführung mit stdout/stderr. Bei Vermittlung phasenweise Ereignisse spiegeln.

**5. Freigeben.** Neue Leases ohne `--keep` freigeben, ausdrücklich mit `--id` wiederverwendete behalten. `--stop-after` überschreibt dies. Anbieter bereinigt, Broker gibt Kosten frei. Scheitert eine frische, nicht behaltene Maschine vor SSH-Bereitschaft, ersetzt `crabbox run` sie einmal. Bei behaltenen oder ausdrücklich wiederverwendeten Leases werden Befehle nicht doppelt ausgeführt.

## Warme Maschinen wiederverwenden {#warm-boxes-and-reuse}

`crabbox warmup` erstellt wie üblich, führt aber keinen Befehl aus. Wiederverwendung per Slug oder ID:

```sh
crabbox warmup --profile project-check --slug blue-lobster
crabbox run --id blue-lobster -- pnpm test:changed
crabbox ssh --id blue-lobster
crabbox stop blue-lobster
```

Jedes Lease hat `cbx_…` und Slug. Automatische Slugs tragen acht Hex-Zeichen wie `blue-lobster-1f3a9c2b`. `--slug` wählt eigene Namen, `--id` akzeptiert beide. Heartbeats aktualisieren `lastTouchedAt` und Leerlaufablauf. Unberührte Leases gibt der dauerhafte Scheduler frei.

## Vermittelt oder direkt {#brokered-vs-direct-provider}

**Vermittelt** ist der normale Team-Weg:

```text
CLI -> coordinator (Cloudflare or Node/PostgreSQL) -> provider API
CLI -> runner over SSH/rsync
```

Geheimnisse bleiben zentral; Bereinigung, Nutzung und Kosten laufen über den Broker. Für `hetzner`, `aws`, `azure`, `daytona`, `gcp` nur mit URL über `CRABBOX_COORDINATOR` oder `config set-broker`.

Ohne URL gilt **direkt**:

```text
CLI -> provider API
CLI -> runner over SSH/rsync
```

Lokale Zugangsdaten wie AWS-SDK-Kette oder Hetzner-Token erforderlich. Keine zentrale Historie oder Broker-Heartbeats. Geeignet zur Brokerdiagnose und für reine Direktadapter wie Sandboxes, statisches SSH und Container.

`broker.mode: registered` bleibt direkt, spiegelt aber eigentümergebundene Metadaten und Heartbeats. Das ergänzt Portal-Inventar, optionales Teilen und ausgehendes WebVNC ohne Übertragung von Zugangsdaten oder Bereinigungsrecht. Freigabe entfernt standardmäßig nur Metadaten; ausdrücklich gebundene ausgehende Adapter können nach Benutzerbestätigung Arbeitsbereiche löschen. Registrierungen zählen nicht zu verwalteten Quoten, Kosten, Images, Pools oder Wartung.

Statisches SSH (`provider: ssh`) umgeht Broker auch bei gesetzter URL. macOS und WSL2 nutzen POSIX-rsync, natives Windows PowerShell/tar.

## Authentifizierung und Identität {#auth-and-identity}

Benutzer nutzen signierte GitHub-Tokens, vertrauenswürdige Automatisierung gemeinsame Bearer-Tokens. Cloudflare Access ist zusätzlich möglich; Node vertraut Identitätsheadern nur aus konfigurierten Reverse-Proxy-CIDRs.

```text
Authorization: Bearer <token>
X-Crabbox-Owner: <email>
X-Crabbox-Org: <org>
```

Bei `crabbox login` ist der Eigentümer das unveränderliche `github:<numeric-id>`. Gemeinsam genutzte Tokens beziehen ihn aus `CRABBOX_OWNER`, Git-Mail-Umgebung oder `git config user.email`; `CRABBOX_ORG` setzt die Organisation. Rohe Access-Header werden ignoriert, nur verifizierte JWT-E-Mail zählt. Siehe [Sicherheit](security.md).

## Synchronisationsmodell {#sync-model}

Lokal zuerst, keine saubere Arbeitskopie erforderlich:

- Git aus Origin/Basis vorbereiten;
- Änderungen per rsync, optional mit Prüfsumme;
- No-op durch Fingerprints überspringen;
- große Verzeichnisse ausschließen;
- verdächtige Massenlöschungen erkennen;
- Basishistorie für Änderungstests vorbereiten.

Menschen und Agenten bearbeiten lokal und führen remote aus. `crabbox sync-plan` ist die nur lesende Vorschau.

## Kosten und Nutzung {#cost-and-usage}

Zwei Kostenwerte pro Lease:

```text
estimatedUSD   elapsed runtime cost so far
reservedUSD    worst-case TTL cost reserved before provisioning
```

Reihenfolge der Stundenpreise:

```text
1. CRABBOX_COST_RATES_JSON   explicit override
2. provider live pricing     e.g. EC2 spot history, Hetzner server-type prices
3. built-in fallback rates   last-resort defaults
```

Hetzner wird über `CRABBOX_EUR_TO_USD` umgerechnet. `CRABBOX_MAX_*` begrenzt aktive Leases und monatliche USD global sowie pro Eigentümer/Organisation; Überschreiten liefert HTTP 429. `crabbox usage` fragt `GET /v1/usage` ab und gruppiert nach Nutzer, Organisation, Anbieter, Typ.

## Ausfälle und Bereinigung {#failure-and-cleanup}

Abstürze, SSH-Abbruch, cloud-init-Fehler, Teilerfolge, Wiederholungen und langlebigere Maschinen werden erwartet:

- Entscheidungen serialisieren;
- möglichst idempotent erstellen;
- Crabbox-Tags;
- wiederholbar sichere Freigabe;
- Durable-Object-Alarme oder pg-boss plus Abgleich;
- konservative direkte Bereinigung;
- Broker besitzt vermittelte Bereinigung.

Mit Koordinator lehnt `crabbox cleanup` Ressourcenbereinigung ab: Sie gehört dem Koordinator.

## Weiterführend {#where-to-go-next}

- [Architektur](architecture.md): Komponenten, API, Zustand, Fehler.
- [Orchestrator](orchestrator.md): Verantwortung, Lebenszyklus, Kosten.
- [CLI](cli.md): Befehle, Konfiguration, Ausgabe, Codes.
- [Funktionen](features/README.md): einzelne Bereiche.
- [Befehle](commands/README.md): Einzelreferenzen.
- [Infrastruktur](infrastructure.md): Laufzeiten, Eingang, Geheimnisse, DNS, Einrichtung.
- [Sicherheit](security.md): Authentifizierung, SSH, Bereinigung und Vertrauen.
