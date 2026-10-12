<!-- source: use-cases.md; sha256: 769c5e58a1b095392245d30f0377bf87b025fa28c3eeca64f9337340d43cd3d7 -->
# Anwendungsfälle {#use-cases}

Gib jedem Coding-Agenten einen Computer auf Abruf für Tests und Builds. Die Agenten bearbeiten ihre lokalen Arbeitskopien weiter, während Crabbox nicht eingecheckte Änderungen synchronisiert, Befehle remote ausführt und Ausgabe sowie Exitcodes zurückgibt. Getrennte Remote-Leases verhindern Konkurrenz um CPU, Speicher und Ports des Laptops.

Wähle Linux, macOS, natives Windows oder WSL2, wenn Plattformverhalten wichtig ist. Für wiederholte Läufe dienen warme Maschinen; zum Prüfen der Ergebnisse kannst du einen Desktop oder Browsereditor öffnen. Der [Leitfaden zu Desktop, VNC, Browser, Code und Portal](features/interactive-desktop-vnc.md) erklärt interaktiven Zugriff und Übergaben.

Gehe von der Aufgabe aus, nicht vom Anbieter:

```sh
crabbox providers recommend
crabbox providers recommend fast-feedback
crabbox providers recommend agent-sandbox --json
```

Ohne Anwendungsfall listet `crabbox providers recommend` alle unterstützten Empfehlungsnamen. Empfehlungen verwenden eingecheckte Fähigkeitsmetadaten. Sie kontaktieren keine Anbieter, prüfen keine Zugangsdaten oder Kontingente, messen keine Startzeiten und zertifizieren keine Sicherheitsgrenze.

## Test- und Build-Schleifen beschleunigen {#speed-up-test-and-build-loops}

Für Repository-Befehle, die lokal zu langsam, speicherhungrig oder störend sind.

```sh
crabbox providers recommend fast-feedback
crabbox providers recommend linux-vm
crabbox run --provider hetzner -- pnpm test
```

Nutze `fast-feedback` für lokale Laufzeiten, wiederverwendbare Caches und kurze Bearbeitungszyklen. Nutze `linux-vm`, wenn die Ausführung den Arbeitsplatz verlassen soll oder vollständige VM- und SSH-Semantik erforderlich ist.

## Einem Coding-Agenten eine Wegwerfumgebung geben {#give-a-coding-agent-a-disposable-environment}

Verwende ein Remote-SSH-Lease oder eine delegierte Sandbox für Repository-Befehle außerhalb des Arbeitsplatzes. Eine lokale Sandbox trennt den Arbeitsbereich, verbraucht aber weiterhin Laptop-Ressourcen. Crabbox übernimmt Ausführung und Bereinigung; der aufrufende Agent oder seine Laufzeitsteuerung übernimmt die Orchestrierung.

Erstelle für unabhängige Agenten jeweils aus deren Arbeitskopie ein eigenes Lease:

```sh
crabbox warmup --provider hetzner --class standard --slug agent-tests
crabbox run --id agent-tests -- pnpm test
crabbox stop agent-tests
```

Nutze unterschiedliche Slugs. Warme Leases behalten die Einrichtung zwischen Befehlen; prüfe die Repository-Konfiguration und installiere Werkzeuge und Abhängigkeiten vor den Tests.

```sh
crabbox providers recommend agent-sandbox
crabbox providers recommend disposable-execution
crabbox providers recommend isolated-execution
```

Diese drei Empfehlungen beantworten unterschiedliche Fragen:

- `agent-sandbox` bevorzugt Anbieter für Agenten- und Devbox-Ausführung;
- `disposable-execution` verlangt eine bereinigbare temporäre Laufzeit;
- `isolated-execution` bevorzugt delegierte und lokale Sandbox-Grenzen.

Die letzte Empfehlung ist eine Routinghilfe, keine Sicherheitszertifizierung. Crabbox setzt vertrauenswürdige Betreiber, Repository-Konfiguration und Aufgaben voraus. Wegwerfmaschinen machen bösartigen Code nicht sicher. Lies das Vertrauens- und Netzwerkmodell vor der Weitergabe von Geheimnissen.

## Linux, Windows, WSL2 oder macOS validieren {#validate-linux-windows-wsl2-or-macos}

Verwende Plattformempfehlungen, wenn das Verhalten vom Zielbetriebssystem abhängt:

```sh
crabbox providers recommend linux-vm
crabbox providers recommend windows
crabbox providers recommend macos

crabbox run --provider aws --target windows --windows-mode wsl2 -- uname -a
```

Natives Windows führt PowerShell und Windows-Werkzeuge aus. WSL2 nutzt den POSIX-Vertrag in einer Windows-VM mit verschachtelter Virtualisierung. macOS-Ziele können lokale Apple Silicon VMs oder Cloud-Macs sein, abhängig vom Anbieter.

## Browser oder sichtbaren Desktop testen {#test-a-browser-or-visible-desktop}

Nutze ein desktopfähiges SSH-Lease für Browser, Screenshots, Eingaben oder Übergaben an Teammitglieder:

```sh
crabbox providers recommend desktop
crabbox warmup --provider azure --target windows --windows-mode normal --desktop --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

WebVNC erlaubt berechtigten Teammitgliedern, den Desktop zu sehen und zu übernehmen. Natives VNC nutzt einen SSH-Tunnel; `crabbox code` öffnet code-server auf unterstützten Linux-Leases. Das Portal zeigt Leases, Bridge-Zustand und gespeicherte Nachweise. Halte die lokale Bridge während der Browsernutzung aktiv. Voraussetzungen stehen im [gemeinsamen Zugriffsleitfaden](features/interactive-desktop-vnc.md).

Ergänze Nachweis-Routing, wenn Ergebnisse nach Freigabe der Maschine erhalten bleiben müssen:

```sh
crabbox providers recommend run-evidence
crabbox providers recommend failure-diagnostics
```

## Warmen Zustand wiederverwenden {#reuse-warm-state}

Nutze warme Maschinen, wenn erneutes Einrichten teurer ist als das Behalten oder Wiederherstellen vorbereiteten Zustands:

```sh
crabbox providers recommend warm-start
crabbox providers recommend versioned-workspace

crabbox warmup --slug warm-tests
crabbox run --id warm-tests -- pnpm test:changed
```

`warm-start` berücksichtigt lokale Laufzeiten, Cache-Volumes, erhaltene Sitzungen, Pause/Fortsetzen und Arbeitsbereichszustand. Es verspricht nicht bei jedem Anbieter dieselben Snapshot- oder Warm-Pool-Funktionen.

## Parallele Experimente auffächern {#fan-out-parallel-experiments}

Nutze forkfähige Arbeitsbereiche für parallele Branches, mehrere Agentenversuche oder Test-Shards aus vorbereitetem Zustand:

```sh
crabbox providers recommend fanout-testing
crabbox checkpoint fork <checkpoint-id> --count 4
```

Dies ist anbieterneutrales Checkpoint- und Fork-Routing, keine Garantie für das Forken des laufenden Speichers einer microVM.

## GPU-Aufgaben ausführen {#run-gpu-workloads}

Nutze GPU-Empfehlungen für Modelltests, CUDA-Builds, Rendering und andere beschleunigte Aufgaben:

```sh
crabbox providers recommend gpu
crabbox run --provider runpod -- nvidia-smi
```

GPU-Anbieter mit SSH-Leases eignen sich meist besser für interaktives Debugging. Delegierte GPU-Anbieter sind oft passender, wenn der Anbieter Ausführung und Ergebnissammlung übernehmen soll.

## Lokal ohne Cloud-Zugangsdaten ausführen {#run-locally-without-cloud-credentials}

Verwende lokale Container, VMs oder Policy-Sandboxes für schnelle Smoke-Tests auf dem Arbeitsplatz:

```sh
crabbox providers recommend local
crabbox providers recommend offline-validation
crabbox run --provider local-container -- pnpm test
```

Lokal bedeutet nicht dasselbe Isolationsmodell. Docker-kompatible Container, Apple VM, Multipass VM, Windows Sandbox und lokale Policy-Laufzeiten unterscheiden sich bei Hostzugriff, Persistenz und Bereinigung. Lies vor unbekanntem Code die Anbieterseite.

## Vorhandene Infrastruktur nutzen {#use-infrastructure-you-already-own}

Crabbox kann dauerhafte SSH-Hosts, private Virtualisierung oder direkte Cloud-Konten ohne gemeinsamen Koordinator verwenden:

```sh
crabbox providers recommend byo-ssh
crabbox providers recommend self-hosted
crabbox providers recommend linux-vm
```

Nutze `ssh`, wenn Crabbox den Host-Lebenszyklus nicht besitzen darf. Nutze selbst gehostete Anbieter wie Proxmox, XCP-ng, Incus, KubeVirt oder Firecracker, wenn Crabbox die Laufzeit erstellen und bereinigen soll.

## Eine kontrollierte Team-Flotte teilen {#share-a-governed-team-fleet}

Der Koordinator ermöglicht gemeinsame Zugangsdaten, Lease-Eigentum, Bereinigung, Nutzungsprotokolle, aktive Lease-Limits und monatliche Ausgabenlimits:

```sh
crabbox providers recommend team-cloud
crabbox login --url https://broker.example.com
crabbox usage --scope user
```

Der Koordinator ist eine Steuerungsebene. Die CLI synchronisiert und führt Befehle weiterhin direkt auf normalen SSH-Runnern aus und speichert Nachweise beim Koordinator. Unter `/portal` findest du Lease-Inventar und Links zu Desktops, Editoren und Läufen. Registrierungen direkter Anbieter ergänzen Sichtbarkeit, ohne Zugangsdaten oder Bereinigungsverantwortung zu übertragen.

## Einen spezielleren Weg finden {#need-a-more-specific-route}

Die CLI empfiehlt auch Anbieter für Artefakte, CI-Nachweise, Code-Interpreter, Fehlerdiagnose, MCP-Anbindung, Netzwerkbegrenzung, Pause/Fortsetzen, Vorschau-URLs, Erreichbarkeit, Remote-Entwicklung, Ressourcenbeobachtung, wiederverwendbare Sitzungen, Web-Smoke-Tests und Worker-/Modulausführung.

Die aktuelle Liste mit Aliasen, Filtern, Bewertungen und Einschränkungen steht unter [`crabbox providers recommend`](commands/providers.md#providers-recommend).

## Verwandte Dokumentation {#related-docs}

- [Anbieterauswahl](features/provider-selection.md)
- [Anbieterreferenz](providers/README.md)
- [Verschachtelte Ausführung](features/nested-execution.md)
- [Preise und Kosten](pricing.md)
