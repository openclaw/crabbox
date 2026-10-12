<!-- source: concepts.md; sha256: 27d2835bb5b9d8ffdb9a39d681b5f2c01215e10c33a0199b1b1e2dd3347e29c8 -->
# Concepts {#concepts}

Crabbox fournit des ordinateurs aux agents et humains. Vous modifiez localement ; une **réservation** retient une machine et une **exécution** synchronise puis lance une commande. Des machines distinctes permettent des tests parallèles sans partager CPU, mémoire ou ports.

Cibles : Linux, macOS, Windows natif, WSL2. Les réservations prêtes conservent l’installation ; Desktop, Browser, Code et portail donnent accès et preuves. Voir le [guide d’accès](features/interactive-desktop-vnc.md).

Ce glossaire est commun à la CLI, au coordinateur et aux documents. Les synonymes préférés sont en **gras**.

## Vocabulaire du calcul {#compute-vocabulary}

**Lease (réservation) :** réservation temporaire d’un exécuteur créé/résolu, avec `cbx_...`, slug, inactivité, TTL et état `active`, `released`, `expired`, `failed`. Unité de coût et nettoyage.

**Runner (exécuteur) :** machine préparée par le fournisseur pour une ou plusieurs exécutions : Hetzner, AWS EC2 ou SSH statique selon le backend.

**Box** / **testbox :** synonymes informels. Préférer runner sauf contexte produit comme machine prête.

**Pool :** ensemble actif visible pour utilisateur, organisation ou flotte ; `crabbox list` et `GET /v1/pool` administratif.

**Pond :** groupe spontané portant `pond=<name>`, sans objet central. Existe tant qu’un membre actif demeure. `crabbox pond peers`, `crabbox pond
connect`/`disconnect`, `crabbox pond release`. Voir [Pond](features/pond.md).

**Lease ID :** `cbx_` plus 12 chiffres hexadécimaux minuscules, 16 caractères, comme `cbx_abcdef123456`, dans étiquettes, journaux, déclarations et API. [Identifiants](features/identifiers.md).

**Slug :** nom lisible, mots plus huit chiffres d’empreinte, comme `swift-crab-1f3a9c2b`, avec suffixe de quatre en cas de collision. `--slug swift-crab` personnalise ; `--slug` normalise en `[a-z0-9-]`, 41 caractères maximum. `--id` accepte les deux.

**Run :** appel `crabbox run`, ID `run_`, réservation, commande, code de retour. Historique durable côté coordinateur, identifiants locaux dans mesures/preuves/échecs directs. [Historique et journaux](features/history-logs.md).

**Workspace :** copie et état : fichiers, dépendances, répertoire fournisseur, sortie, bureau/navigateur optionnel. À préférer lorsque le produit dépasse la machine brute.

## Rôles {#roles}

**CLI :** binaire Go local `crabbox`, responsable de configuration, synchronisation, exécution, sortie et clés SSH. [Architecture](architecture.md).

**Broker** / **coordinator :** plan partagé authentifié, identifiants, état, expiration, nettoyage, historique, usage, plafonds. Cloudflare Workers/Fleet Durable Object ou Node.js/PostgreSQL/pg-boss. Coordinator souligne le service ; broker la frontière de confiance.

**Provider :** adaptateur d’acquisition, résolution, inventaire et libération. Clouds AWS, Azure, Google Cloud, Hetzner ; Proxmox, Parallels, Local Container, SSH ; sandboxes SSH Daytona, KubeVirt, External, Namespace Devbox, Semaphore, Sprites, exe.dev, RunPod, Railway ; délégués Blacksmith Testbox, E2B, Modal, Islo, Tensorlake, [Upstash Box](https://upstash.com/docs/box/overall/quickstart), Cloudflare, Azure Dynamic Sessions, W&B. [Référence](providers/README.md).

**Backend :** interface Go `SSHLeaseBackend` pour cible SSH réelle ou `DelegatedRunBackend` pour exécution contrôlée par le fournisseur. [Backends](provider-backends.md).

**Operator :** personne disposant d’un accès administratif ; exécute `crabbox admin` et les opérations de création/promotion d’images.

**Agent :** processus LLM appelant la CLI. Crabbox donne exécution et preuves ; agent/harnais possède prompts, décisions et orchestration. Configuration et charges doivent être fiables.

## Modes {#modes}

**Brokered mode** / **coordinator mode :** coordinateur pour création/état/nettoyage, CLI pour SSH/rsync/exécution directe. Secrets centraux. AWS, Azure, Daytona, Google Cloud, Hetzner via `CRABBOX_COORDINATOR` ou `config set-broker`.

**Direct mode** / **direct-provider mode :** API directe sans historique central ni plafonds. Obligatoire hors de ces cinq, utilisé aussi par ceux-ci sans URL.

**Static mode :** `provider: ssh`, hôte de l’opérateur, ni créé ni supprimé par Crabbox, toujours direct.

**Delegated mode :** Blacksmith, E2B, Modal, Islo et autres exécutent et renvoient la sortie. Rejette `--sync-only` et `--checksum` locaux ; indique `sync=delegated`.

## Commandes {#commands}

**warmup :** réserve et prépare, `--keep` vrai par défaut, aucune commande encore.

**run :** réserve/réutilise, synchronise, exécute, renvoie sortie/code. Nouvelles réservations libérées, réutilisées conservées ; contrôle par `--keep` et `--stop-after`.

**stop** / **release :** termine selon le contrat ; ressources jetables généralement supprimées, hôtes statiques et ressources retenues exceptés.

**cleanup :** nettoie les restes directs admissibles après vérification précise de propriété. Nom, ID ou étiquette seuls ne suffisent pas.

**reuse :** sélection via `--id`/slug pour `run`. `warmup` crée ; `--lease-id` assure une création idempotente, pas une réutilisation universelle.

**reclaim :** déplace la déclaration entre copies (`--reclaim`) pour réutiliser depuis un autre dépôt, car les réservations sont liées par défaut.

**hydrate :** prépare les dépendances depuis les Actions du dépôt. Étapes compatibles directement ; `--github-runner` enregistre l’hôte et déclenche une vraie tâche pour la sémantique complète. [Préparation Actions](features/actions-hydration.md).

## État {#state}

**Idle timeout :** délai sans battement, 1800 s/30 min par défaut, réinitialisé par battement ou touch.

**TTL :** durée absolue 5400 s/90 min, plafond 86400 s/24 h, non prolongeable par battement. `expiresAt = min(createdAt + ttl, lastTouchedAt + idleTimeout)`.

**Heartbeat :** `POST /v1/leases/{id}/heartbeat` met à jour `lastTouchedAt`, transporte éventuellement la télémétrie et change l’inactivité sur demande. Refus à partir de `expiresAt` ; le nettoyage prend la main.

**Touch :** mise à jour état/inactivité. `Touch` du fournisseur pour le direct, battement pour l’intermédié.

**Reserved cost :** maximum `hourlyRate × ttl`, réservé contre le budget mensuel jusqu’à libération.

**Estimated cost :** prix horaire multiplié par durée `active`, estimation dans `crabbox usage`. [Coût et usage](features/cost-usage.md).

## Synchronisation {#sync}

**Manifest :** chemins séparés par NUL depuis `git ls-files --cached` et `git ls-files --others --exclude-standard`, suppressions séparées.

**Fingerprint :** hash du commit, métadonnées modifiées et manifeste ; égalité évite rsync.

**Git seeding :** préparation initiale optionnelle de Git distant depuis origin/base pour permettre les différences distantes.

**Base ref :** référence de préparation/hydratation définie avec `sync.baseRef`.

**Sanity check :** après rsync, détection de suppressions massives, fichiers absents et résultats suspects. [Synchronisation](features/sync.md).

## Capacités {#capabilities}

**Desktop :** `--desktop` ajoute TigerVNC/XFCE redimensionnable sur Linux géré et nouveaux conteneurs, ou Wayland/GNOME. Nécessaire pour `crabbox
vnc`, `crabbox webvnc`, et la plupart des tests visuels `--browser`.

**Browser :** `--browser` installe/découvre un navigateur et exporte son chemin. Sans interface possible, `--desktop` pour l’affichage. Moteur selon fournisseur/image, sans profil connecté.

**Code :** `--code` installe code-server sur loopback 8080 pour `crabbox code` et `/code/`, Linux géré seulement.

**Tailscale :** accès optionnel au Linux géré par tailnet sans IP publique, distinct du choix de plan `--network tailscale`. [Capacités](features/capabilities.md).

## Environnement du coordinateur {#coordinator-runtime}

**FleetCoordinator :** logique TypeScript commune : routes, identité, adaptateurs, registres, coûts, nettoyage, portail, ponts.

**Durable Object :** état Cloudflare et décisions sérialisées ; un objet Fleet `idFromName("default")`.

**PostgreSQL runtime :** Node portable, schéma `crabbox` pour clés/valeurs, `crabbox_jobs` de pg-boss pour maintenance précise et périodique.

**Alarm :** échéance future de nettoyage, via Durable Object ou pg-boss pour inactivité, TTL, reprises et maintenance.

**Portal :** interface authentifiée rendue côté serveur sous `/portal/...`, réservations, journaux, événements, VNC/Code. [Portail](features/portal.md).

**Bridge :** lien vers le relais CLI local. WebVNC/Code maintiennent SSH vers VNC loopback 5900/code-server 8080 et doivent rester actifs. Egress utilise un relais distinct à tickets. Le coordinateur authentifie, sans ouvrir SSH lui-même.

## Identité {#identity}

**Owner :** propriétaire ; GitHub fixe `github:<numeric-id>`. Automatisation partagée : propriétaire configuré ou email Git selon routage vérifié. [Sécurité opérationnelle](security.md#authentication).

**Org :** organisation issue du token ou de `CRABBOX_ORG`, périmètre d’usage et limites multilocataires.

**Allowed org :** appartenance requise avant émission du token signé, configurée par Worker.

**Admin token :** périmètre séparé pour `GET /v1/pool`, routes admin et inventaire global ; plus protégé que le token partagé.

**Cloudflare Access :** couche optionnelle ; confiance dans `cf-access-jwt-assertion` vérifié, pas dans les en-têtes bruts.

## Stockage {#storage}

**State directory :** déclarations, historique, checkpoints. `XDG_STATE_HOME` choisit `$XDG_STATE_HOME/crabbox` aussi pour clés/confiance. Sinon `<os-user-config-dir>/crabbox/state`, clés dans `<os-user-config-dir>/crabbox/testboxes`. Changer de racine ne migre ni ne recherche l’ancien état. [Clés SSH](features/ssh-keys.md).

**Claim :** JSON liant réservation et copie, utilisé par `crabbox run --id` pour les slugs et le refus inter-dépôts sans `--reclaim`.

**Workdir** / **work root :** destination de synchronisation, `/work/crabbox` Linux, `C:\crabbox` Windows normal, sinon selon cible/fournisseur.

## Preuves et artefacts {#evidence-and-artifacts}

**Run history :** historique (`crabbox history`, `logs`, `events`, `results`), distinct de l’état courant. [Historique](features/history-logs.md).

**Test results :** JUnit via `crabbox results <run-id>` : suites, tests, échecs, erreurs, ignorés. [Résultats](features/test-results.md).

**Telemetry :** échantillons Linux de charge, mémoire, disque et uptime autour du run, pas analytique produit. [Télémétrie](features/telemetry.md).

**Artifacts :** `crabbox artifacts` produit captures, vidéo, GIF, journaux, état et métadonnées publiables dans une PR. [Artefacts](features/artifacts.md).

**Egress :** `crabbox egress` transporte le réseau sortant par la machine opérateur via WebSocket. [Egress](features/egress.md).

**Checkpoint :** état VM/workspace restaurable ou forkable, ID `chk_...`, archives, recettes, snapshots/images natifs. [Checkpoints](features/checkpoints.md).

**Cache volume :** volume persistant selon dépôt/runtime/plateforme, chemin absolu, seulement pour accélérer. Ni scénario forkable ni source de vérité ; l’arbre de travail reste maître. [Cache](features/cache.md).

**Capsule :** échec portable capturé par `crabbox capsule from-actions` et rejoué par `crabbox capsule
replay`. [Capsules](features/capsules.md).

## Documentation {#documentation}

**Source map :** relie comportement et implémentation. [Carte](source-map.md).

**Feature page :** `docs/features/<name>.md`, histoire conceptuelle liée aux commandes/fournisseurs.

**Command page :** `docs/commands/<name>.md`, options, comportement, codes synchronisés avec `--help`.

**Provider page :** `docs/providers/<name>.md`, cibles, configuration, variables, sync et échecs.

Documents associés :

- [Fonctionnement](how-it-works.md)
- [Architecture](architecture.md)
- [CLI](cli.md)
- [Configuration](features/configuration.md)
- [Backends](provider-backends.md)
