<!-- source: concepts.md; sha256: 27d2835bb5b9d8ffdb9a39d681b5f2c01215e10c33a0199b1b1e2dd3347e29c8 -->
# Begriffe {#concepts}

Crabbox stellt Agenten und Menschen Computer bereit. Lokal bearbeiten, per **Lease** reservieren, per **Run** synchronisieren und ausführen. Getrennte Maschinen ermöglichen parallele Tests ohne Konkurrenz um CPU, Speicher und Ports.

Ziele sind Linux, macOS, natives Windows und WSL2. Warme Leases bewahren Einrichtung; Desktop, Browser, Code und Portal bieten Zugriff und Nachweise. Siehe [Zugriffsleitfaden](features/interactive-desktop-vnc.md).

Dieses Glossar gilt für CLI, Koordinator und Dokumentation. Bevorzugte Synonyme sind **fett**.

## Rechenbegriffe {#compute-vocabulary}

**Lease:** zeitbegrenzte Reservierung mit `cbx_...`, Slug, Leerlauf, TTL und Zustand `active`, `released`, `expired`, `failed`. Einheit von Abrechnung und Bereinigung.

**Runner:** vom Anbieter bereitgestellte und vorbereitete Maschine für einen oder mehrere Läufe, ob Hetzner, AWS EC2 oder statischer SSH-Host.

**Box** / **testbox:** informell für Runner. In neuer Doku Runner bevorzugen, außer bei produktbezogenen Begriffen wie warme Box.

**Pool:** aktive Runner für Benutzer, Organisation oder Flotte; `crabbox list` und administratives `GET /v1/pool`.

**Pond:** Gruppe mit Anbieterlabel `pond=<name>`, kein zentrales Clusterobjekt. Existiert mit mindestens einem aktiven Mitglied. `crabbox pond peers`, `crabbox pond
connect`/`disconnect`, `crabbox pond release`; siehe [Pond](features/pond.md).

**Lease ID:** `cbx_` plus zwölf kleine Hex-Zeichen, insgesamt 16, etwa `cbx_abcdef123456`; in Labels, Logs, Claims und APIs. [Kennungen](features/identifiers.md).

**Slug:** lesbarer Name aus Wortpaar und acht Hex-Zeichen, z. B. `swift-crab-1f3a9c2b`; Kollisionen ergänzen vier. `--slug swift-crab` erlaubt eigene Namen. `--slug` normalisiert zu `[a-z0-9-]`, höchstens 41 Zeichen. `--id` akzeptiert Name oder ID.

**Run:** Aufruf von `crabbox run` mit `run_`, Lease, Befehl und Exitcode. Zentral dauerhafte Historie, direkt IDs in lokalen Zeit-/Fehlernachweisen. [Historie und Logs](features/history-logs.md).

**Workspace:** Arbeitskopie plus Laufzeitzustand: Dateien, Abhängigkeiten, Arbeitsverzeichnis, Ausgabe, optional Desktop/Browser. Verwenden, wenn mehr als die Maschine gemeint ist.

## Rollen {#roles}

**CLI:** lokales Go-Programm `crabbox`; Konfiguration, Sync, Ausführung, Ausgabe und SSH-Schlüssel. [Architektur](architecture.md).

**Broker** / **coordinator:** authentifizierte gemeinsame Steuerungsebene mit Zugangsdaten, Leases, Ablauf, Bereinigung, Historie, Nutzung und Limits. Cloudflare Workers/Fleet Durable Object oder Node.js/PostgreSQL/pg-boss. Coordinator betont Dienst, Broker die Vertrauensgrenze.

**Provider:** Adapter für Beschaffen, Auflösen, Auflisten, Freigeben. Clouds AWS, Azure, Google Cloud, Hetzner; Proxmox, Parallels, Local Container, SSH; SSH-Sandboxes Daytona, KubeVirt, External, Namespace Devbox, Semaphore, Sprites, exe.dev, RunPod, Railway; delegiert Blacksmith Testbox, E2B, Modal, Islo, Tensorlake, [Upstash Box](https://upstash.com/docs/box/overall/quickstart), Cloudflare, Azure Dynamic Sessions, W&B. [Referenz](providers/README.md).

**Backend:** Go-Schnittstelle `SSHLeaseBackend` für reale SSH-Ziele oder `DelegatedRunBackend` für anbietereigene Ausführung. [Backends](provider-backends.md).

**Operator:** Person mit Broker-Adminzugriff, führt `crabbox admin` und Image-Erstellung/-Promotion aus.

**Agent:** LLM-Prozess als CLI-Aufrufer. Crabbox liefert Ausführung/Nachweise, Agent oder Harness verantwortet Prompts, Entscheidungen und Orchestrierung. Konfiguration und Aufgabe müssen vertrauenswürdig sein.

## Modi {#modes}

**Brokered mode** / **coordinator mode:** Koordinator erstellt, verwaltet und bereinigt; CLI synchronisiert/führt direkt über SSH aus. Geheimnisse bleiben zentral. AWS, Azure, Daytona, Google Cloud, Hetzner mit `CRABBOX_COORDINATOR` oder `config set-broker`.

**Direct mode** / **direct-provider mode:** direkte API, ohne zentrale Historie oder Kostenlimit. Für alle anderen Anbieter erforderlich und für diese fünf ohne Broker-URL der Standard.

**Static mode:** `provider: ssh`; Betreiberhost, keine Erstellung/Löschung, immer direkt.

**Delegated mode:** Blacksmith, E2B, Modal, Islo usw. führen aus und streamen zurück. Lokale Sync-Optionen `--sync-only`, `--checksum` werden abgelehnt, Zeitangabe `sync=delegated`.

## Befehle {#commands}

**warmup:** mieten und bereithalten, `--keep` standardmäßig wahr, noch kein Befehl.

**run:** mieten/wiederverwenden, synchronisieren, ausführen, Ausgabe/Exitcode. Neue Leases freigeben, wiederverwendete behalten; `--keep` und `--stop-after` steuern dies.

**stop** / **release:** nach Anbietervertrag beenden. Wegwerfressourcen meist löschen, statische und bewusst behaltene ausgenommen.

**cleanup:** geeignete direkte Überreste nach genauer Eigentumsprüfung beseitigen. Label, Name oder ID allein genügen nie.

**reuse:** bestehendes Lease per `--id`/Slug für `run`. `warmup` erstellt; `--lease-id` ist idempotente Erstellung, kein allgemeiner Wiederverwendungsparameter.

**reclaim:** lokalen Claim zwischen Arbeitskopien verschieben (`--reclaim`), da Leases normalerweise an ein Repository gebunden sind.

**hydrate:** Abhängigkeiten aus Repository-Actions vorbereiten. Unterstützte Setup-Schritte direkt; `--github-runner` registriert eigenen Runner und startet echte Actions für volle Semantik. [Actions-Vorbereitung](features/actions-hydration.md).

## Zustand {#state}

**Idle timeout:** Zeit ohne Heartbeat, standardmäßig 1800 s/30 min, durch Heartbeat oder Touch zurückgesetzt.

**TTL:** absolute Lebensdauer, 5400 s/90 min, maximal 86400 s/24 h, nicht durch Heartbeats verlängerbar. `expiresAt = min(createdAt + ttl, lastTouchedAt + idleTimeout)`.

**Heartbeat:** `POST /v1/leases/{id}/heartbeat` aktualisiert `lastTouchedAt`, optional Telemetrie und angeforderten Leerlauf. Ab `expiresAt` abgelehnt; dann übernimmt Bereinigung.

**Touch:** Zustand/Leerlauf aktualisieren. Anbieter-`Touch` ist direkt, Heartbeat das vermittelte Gegenstück.

**Reserved cost:** maximale TTL-Kosten `hourlyRate × ttl`, gegen Monatsbudget bis zur Freigabe reserviert.

**Estimated cost:** Stundenpreis mal Zeit `active`, Näherung in `crabbox usage`. [Kosten/Nutzung](features/cost-usage.md).

## Synchronisation {#sync}

**Manifest:** NUL-getrennte Pfade aus `git ls-files --cached` und `git ls-files --others --exclude-standard`, Löschungen separat.

**Fingerprint:** Hash aus Commit, Änderungsmetadaten und Manifest; Übereinstimmung überspringt rsync.

**Git seeding:** optionale erste Vorbereitung von Remote-Git aus Origin/Basis für entfernte Diffs.

**Base ref:** Git-Basis für Vorbereitung/Hydrierung, per `sync.baseRef`.

**Sanity check:** prüft nach rsync Massenlöschungen, fehlende Manifestdateien und Auffälligkeiten. [Sync](features/sync.md).

## Fähigkeiten {#capabilities}

**Desktop:** `--desktop` ergänzt skalierbare TigerVNC/XFCE-Sitzung auf verwaltetem Linux/neuen Containern oder Wayland/GNOME. Für `crabbox
vnc`, `crabbox webvnc` und die meisten sichtbaren `--browser`-Läufe erforderlich.

**Browser:** `--browser` installiert/findet Browser und exportiert Pfad. Headless möglich, `--desktop` macht ihn sichtbar. Anbieter/Image bestimmt Engine; kein angemeldetes Profil.

**Code:** `--code` installiert code-server auf Loopback 8080 für `crabbox code` und `/code/`, nur verwaltetes Linux.

**Tailscale:** optionales Tailnet für Zugriff ohne öffentliche IP; anders als die Ebenenauswahl `--network tailscale`. [Fähigkeiten](features/capabilities.md).

## Koordinator-Laufzeit {#coordinator-runtime}

**FleetCoordinator:** gemeinsame TypeScript-Logik für Routen, Authentifizierung, Adapter, Zustand, Kosten, Bereinigung, Portal, Bridges.

**Durable Object:** Cloudflare-Zustand und serielle Entscheidungen; ein Fleet-Objekt mit `idFromName("default")`.

**PostgreSQL runtime:** portables Node.js; `crabbox`-Schema für Schlüssel/Werte, pg-boss mit `crabbox_jobs` für genaue Wartung und Abgleich.

**Alarm:** künftige Bereinigungsfrist, über Durable Object oder pg-boss für Leerlauf, TTL, Wiederholungen und Wartung.

**Portal:** servergerenderte authentifizierte UI unter `/portal/...` für Leases, Logs, Ereignisse und VNC/Code. [Portal](features/portal.md).

**Bridge:** Verbindung zum lokalen CLI-Relay. WebVNC/Code halten SSH-Tunnel zu Loopback 5900/8080 und müssen aktiv bleiben. Egress nutzt eigenes Ticket-Relay. Koordinator authentifiziert, öffnet selbst kein SSH.

## Identität {#identity}

**Owner:** Lease-Eigentümer, GitHub bindet `github:<numeric-id>`. Gemeinsame Tokens verwenden konfigurierte Identität oder Git-Mail nach Verifizierung. [Betriebssicherheit](security.md#authentication).

**Org:** Organisationsnamensraum aus Token oder `CRABBOX_ORG`, für Nutzungs- und Mandantenlimits.

**Allowed org:** erforderliche Mitgliedschaft vor Login-Token-Ausgabe, pro Worker konfiguriert.

**Admin token:** gesonderte Berechtigung für `GET /v1/pool`, Adminrouten, Gesamtlisten; strenger geschützt als gemeinsame Automatisierung.

**Cloudflare Access:** optionale Vorschicht; vertraut verifiziertem `cf-access-jwt-assertion`, nicht rohen Clientheadern.

## Speicherung {#storage}

**State directory:** Claims, Historie, Checkpoints. `XDG_STATE_HOME` wählt `$XDG_STATE_HOME/crabbox` auch für Schlüssel/Hostvertrauen. Sonst `<os-user-config-dir>/crabbox/state`, Schlüssel `<os-user-config-dir>/crabbox/testboxes`. Kein automatisches Migrieren/Suchen alter Wurzeln. [SSH-Schlüssel](features/ssh-keys.md).

**Claim:** JSON bindet Lease an Arbeitskopie. Für Slugs in `crabbox run --id` und Ablehnung fremder Repos ohne `--reclaim`.

**Workdir** / **work root:** Synchronisationsziel, Linux `/work/crabbox`, natives Windows `C:\crabbox`, sonst anbieter-/zielabhängig.

## Nachweise und Artefakte {#evidence-and-artifacts}

**Run history:** vergangene Läufe (`crabbox history`, `logs`, `events`, `results`), getrennt vom Live-Zustand. [Historie](features/history-logs.md).

**Test results:** JUnit in `crabbox results <run-id>`: Suites, Tests, Fehlschläge, Fehler, übersprungen. [Ergebnisse](features/test-results.md).

**Telemetry:** leichte Linux-Proben für Last, Speicher, Disk, Uptime; Laufressourcen, keine Produktanalyse. [Telemetrie](features/telemetry.md).

**Artifacts:** `crabbox artifacts` erstellt Screenshots, Video, GIF, Logs, Zustand, Metadaten für PR-Veröffentlichung. [Artefakte](features/artifacts.md).

**Egress:** `crabbox egress` leitet ausgehenden Verkehr per WebSocket durch die Betreibermaschine. [Egress](features/egress.md).

**Checkpoint:** wiederherstellbarer/forkbarer VM-/Workspace-Zustand `chk_...`, Archive, Rezepte oder native Snapshots/Images. [Checkpoints](features/checkpoints.md).

**Cache volume:** persistenter Cache nach Repo/Laufzeit/Plattform an absolutem Pfad, nur zur Beschleunigung; kein forkbares Szenario, Arbeitsbaum bleibt maßgeblich. [Cache](features/cache.md).

**Capsule:** portabler Fehler aus `crabbox capsule from-actions`, wiederholt mit `crabbox capsule
replay`. [Kapseln](features/capsules.md).

## Dokumentation {#documentation}

**Source map:** verbindet Verhalten und Code. [Quellcodekarte](source-map.md).

**Feature page:** `docs/features/<name>.md`, Konzeptseite, von Befehlen/Anbietern verlinkt.

**Command page:** `docs/commands/<name>.md`, Flags, Verhalten, Exitcodes synchron zu `--help`.

**Provider page:** `docs/providers/<name>.md`, Ziele, Konfiguration, Umgebung, Sync und Fehler.

Verwandte Seiten:

- [Funktionsweise](how-it-works.md)
- [Architektur](architecture.md)
- [CLI](cli.md)
- [Konfiguration](features/configuration.md)
- [Backends](provider-backends.md)
