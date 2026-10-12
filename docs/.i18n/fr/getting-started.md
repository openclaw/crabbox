<!-- source: getting-started.md; sha256: 6f010425dbbffe15196dfbc9cbfce66ea14783090c7babf231211bf13239fc09 -->
# Premiers pas {#getting-started}

Crabbox fournit des ordinateurs à la demande aux agents et aux humains. Continuez à modifier localement : il réserve une machine, synchronise les changements non commités, exécute la commande et renvoie sortie et code de retour. Une exécution ponctuelle libère la machine ; une réservation prête reste disponible.

Tests et compilations distants libèrent CPU, mémoire et ports pour le travail parallèle. Choisissez un fournisseur compatible Linux, macOS, Windows natif ou WSL2. Voir le [guide bureau, VNC, navigateur, Code et portail](features/interactive-desktop-vnc.md).

Ce parcours mène un dépôt fiable jusqu’à `crabbox run -- pnpm test`. Vérifiez configuration et préparation avant exécution. Sans compte, `--provider local-container` utilise Docker ou Podman local ; la [compétence `crabbox-quickstart`](integrations/agents.md#install-through-ecosystem-skill-managers) guide ce parcours sans identifiants.

## Étape 1. Installer {#step-1-install}

```sh
brew install openclaw/tap/crabbox
```

Vérifiez :

```sh
crabbox --version
crabbox doctor
```

`crabbox doctor` affiche une ligne par contrôle. Après installation, le contrôle neutre `git` doit indiquer `ok` ; ceux du fournisseur commencent après sélection. Sans sélection, il affiche `no provider selected`, conserve `source=compiled_default selected=false` et ignore la préparation des identifiants sans échouer. Sélectionnez via configuration, `CRABBOX_PROVIDER` ou `--provider` avant une commande de cycle de vie. Cette sélection est stricte et peut échouer sans identifiants ; comparez avec `crabbox providers recommend`.

Sans Homebrew, téléchargez l’archive macOS, Linux ou Windows sur <https://github.com/openclaw/crabbox/releases>. Les exécutables macOS sont signés et notariés.

Gardez `crabbox-runtime` près du véritable exécutable CLI. Les deux architectures Linux appartiennent au contrôleur correspondant ; copier uniquement la CLI ou mélanger les versions ne conserve pas le paquet complet.

Go permet d’installer seulement la CLI d’une version explicite. Choisissez un tag dans [GitHub Releases](https://github.com/openclaw/crabbox/releases), pris en charge depuis v0.44.0 :

```sh
go install github.com/openclaw/crabbox/cmd/crabbox@v0.70.0
```

Le module exige Go 1.26 et préfère go1.26.5. Utilisez Go 1.26.5 ou plus récent, ou la sélection automatique. Cela compile la CLI sans installer `crabbox-apple-vm-helper`, paquet d’exécution ni autres composants, et ne constitue pas la distribution signée/notariée. Préférez Homebrew ou une archive complète, notamment pour Apple VM sur Apple Silicon.

La sélection des transferts WSL2 et Windows x64 sans WSL exige `main` actuel ou Crabbox v0.42.1 minimum. Voir [Installation Windows](windows-install.md).

## Étape 2. Se connecter {#step-2-log-in}

Pour une flotte partagée, utilisez l’URL du coordinateur d’équipe. Il conserve les identifiants et applique limites, plafonds et expiration. Les fournisseurs directs et locaux ignorent la connexion ; voir [Choisir un accès](#choosing-an-access-path).

```sh
crabbox login --url https://broker.example.com
```

`login` ouvre GitHub OAuth ; `--no-browser` affiche l’URL pour un navigateur du même appareil. Le courtier échange le code, vérifie l’organisation et redirige une confirmation à usage unique vers l’écouteur local de la CLI avant d’enregistrer le token signé :

```text
logged in broker=https://broker.example.com provider=hetzner user=github:12345 org=example-org config=/Users/alice/Library/Application Support/crabbox/config.yaml
```

Ensuite, chaque commande `crabbox` s’authentifie automatiquement. Vérifiez l’identité :

```sh
crabbox whoami
```

```text
user=github:12345 org=example-org auth=user broker=https://broker.example.com
```

### Choisir un accès {#choosing-an-access-path}

L’accès dépend du déploiement. Utilisez l’URL et la liste d’organisations/équipes autorisées de votre équipe. Une connexion OAuth terminée peut être refusée hors de cette liste.

Pour une installation personnelle ou tierce :

- **Fournisseur direct :** identifiants locaux pour des tests privés rapides ; état et nettoyage locaux plutôt qu’historique central et plafonds partagés.
- **Coordinateur autohébergé :** Cloudflare Workers ou Node.js/PostgreSQL pour identifiants centralisés, limites actives/mensuelles, `crabbox usage`, nettoyage durable et point d’accès partagé.
- **Demande d’accès :** seulement si l’opérateur définit un processus pour votre organisation. Un point d’accès d’équipe n’est pas automatiquement public.

Le mode direct ignore `login` :

```sh
crabbox doctor --provider hetzner
crabbox run --provider hetzner -- pnpm test
```

Choisissez un environnement d’hébergement :

- **Cloudflare Workers :** état Durable Object, alarmes, nettoyage programmé et Cloudflare Access optionnel.
- **Node.js/PostgreSQL :** HTTP/WebSocket, PostgreSQL 13+, tâches pg-boss et réplique unique permanente. Considérez-le comme l’environnement portable initial et validez le déploiement avant bascule en production.

Les deux partagent secrets, authentification, budgets, API et portail. Voir [Infrastructure](infrastructure.md#choose-a-coordinator-runtime). La connexion web nécessite une app OAuth et une organisation/équipe autorisée ; l’automatisation par token partagé, non.

Pour CI sans navigateur :

```sh
printf '%s' "$TOKEN" | crabbox login \
  --url https://broker.example.com \
  --provider aws \
  --token-stdin
```

Voir [Authentification et administration](features/auth-admin.md).

## Étape 3. Préparer un dépôt {#step-3-onboard-a-repo}

Dans le dépôt :

```sh
crabbox init
```

`init` écrit trois fichiers. Changez les chemins avec `--config`, `--workflow` ou un ou plusieurs `--skill`, et écrasez les fichiers avec `--force` :

```text
.crabbox.yaml                          repo defaults (profile, class, sync, env)
.github/workflows/crabbox.yml          Actions hydration workflow (optional)
.agents/skills/crabbox/SKILL.md        agent-facing skill instructions
```

Adaptez `.crabbox.yaml` :

- `profile` : nom du parcours ; le modèle utilise `<repo>-check`.
- `class` : `tiny`, `small`, `standard`, `fast`, `large` ou `beast` ; modèle `beast`.
- `sync.exclude` : répertoires à ne jamais envoyer.
- `sync.include` : liste autorisée relative à la racine ; après exclusions, **seuls** ces chemins sont synchronisés.
- `env.allow` : variables visibles par la commande distante.

`--detect` recherche les tests et écrit `jobs.detected`, exécutable avec `crabbox job run detected`.

Prévisualisez :

```sh
crabbox sync-plan
```

`sync-plan` affiche nombre de fichiers, octets et fichiers les plus gros. Si `dist/`, `.cache/` ou de gros actifs inattendus apparaissent, resserrez `sync.exclude`. Cette taille limite le premier transfert.

Les espaces Jujutsu natifs avec backend Git fonctionnent si `jj` est installé. Crabbox capture les modifications et synchronise exactement l’arbre `@`, fusions à plusieurs parents comprises. `sync-plan --json` affiche la révision ; utilisez `run --no-hydrate` avec SSH ordinaire. Les espaces Git/Jujutsu colocalisés suivent Git. Voir [Synchronisation Jujutsu](features/sync.md#jujutsu-workspaces) pour prérequis et refus de sécurité.

## Étape 4. Préparer une machine {#step-4-warm-a-box}

```sh
crabbox warmup --class standard --slug first-tests
```

`warmup` réserve, prépare et attend SSH/outils, conserve la réservation (`--keep` activé par défaut) et affiche résumé, adresse et durée :

```text
leased cbx_abcdef123456 slug=first-tests provider=hetzner server=... type=... ip=203.0.113.10 idle_timeout=30m0s expires=...
ready ssh=crabbox@203.0.113.10:2222 network=public workroot=/work/crabbox
warmup complete total=...
```

Le coordinateur maintient la réservation prête. Les valeurs par défaut sont 30 minutes d’inactivité et 90 minutes de TTL. Les fournisseurs directs appliquent leurs propres règles ; arrêtez explicitement après usage. Consultez `crabbox providers --json` avant de choisir une taille propre au fournisseur.

Réutilisez par `slug` ou `id` (`cbx_...`), tous deux acceptés par `--id`.

## Étape 5. Exécuter une commande {#step-5-run-a-command}

```sh
crabbox run --id first-tests -- pnpm test
```

Déroulement :

1. Vérification de SSH.
2. Initialisation Git depuis origin et la référence de base, puis superposition rsync des changements. Une empreinte identique évite le transfert.
3. Exécution SSH et diffusion stdout/stderr.
4. Battements de cœur pour éviter l’expiration pendant le travail.
5. Historique `run_...` avec durées, code de retour et télémétrie limitée sur Linux.

Le dépôt reste responsable des outils et dépendances. Exécutez sa préparation ou [hydratez depuis Actions](features/actions-hydration.md) avant les tests.

Sans `--id`, l’exécution est ponctuelle :

```sh
crabbox run -- pnpm test
```

Une nouvelle réservation est acquise puis libérée à la fin. Pour itérer, utilisez `warmup` + `--id`.

## Étape 6. Examiner l’historique {#step-6-inspect-history}

```sh
crabbox history
crabbox events run_abcdef123456
crabbox logs run_abcdef123456
crabbox results run_abcdef123456
```

`history` liste les exécutions ; `events` affiche réservation, synchronisation, commande, sortie et fin dans l’ordre ; `logs` renvoie la sortie conservée ; `results` analyse les rapports JUnit joints.

Le portail activé présente les mêmes données sur `/portal/runs/run_abcdef123456`.

## Étape 7. Arrêter la réservation {#step-7-stop-the-lease}

Après usage :

```sh
crabbox stop first-tests
```

`stop` applique le nettoyage du fournisseur et libère le coût réservé. Les machines jetables sont généralement supprimées ; les hôtes SSH statiques et ressources retenues suivent leurs règles. La propriété doit être vérifiée. Des déclarations de récupération ou reçus terminaux d’ID fixe peuvent rester localement. Le coordinateur programme le nettoyage des machines oubliées.

```sh
crabbox cleanup --dry-run
```

`cleanup` balaie les restes directs et l’état local. Le nettoyage intermédié appartient aux alarmes du courtier.

## Variantes courantes {#common-variations}

Conserver une longue session :

```sh
crabbox warmup --idle-timeout 4h --ttl 8h --slug swift-crab
crabbox run --id swift-crab -- pnpm test
crabbox run --id swift-crab -- pnpm bench
crabbox stop swift-crab
```

Ouvrir un bureau :

```sh
crabbox warmup --desktop --browser --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

Ouvrir code-server :

```sh
crabbox warmup --code --slug code-check
crabbox run --id code-check --sync-only
crabbox code --id code-check --open
```

Gardez les ponts actifs pendant l’utilisation web. Desktop et Code dépendent du fournisseur et de la cible ; Code web exige aussi coordinateur et origine isolée. Le [guide combiné](features/interactive-desktop-vnc.md) décrit VNC, WebVNC, portail, partage et nettoyage.

Ouvrir la copie synchronisée dans Zed Remote Projects :

```sh
crabbox run --id swift-crab --sync-only
crabbox open --editor=zed --id swift-crab
```

Collez la commande SSH dans **Connect New Server**, ouvrez le dossier indiqué et gardez `crabbox open` actif.

Utiliser un Mac existant (SSH statique, sans création) :

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

Remplacer fournisseur et classe par commande :

```sh
crabbox run --provider aws --class beast -- pnpm test
```

## Pour continuer {#where-to-go-next}

- [Fonctionnement](how-it-works.md) — modèle mental.
- [CLI](cli.md) — commandes et codes.
- [Commandes](commands/README.md) — références individuelles.
- [Fonctions](features/README.md) — par capacité.
- [Configuration](features/configuration.md) — YAML et priorité.
- [Fournisseurs](providers/README.md) — choisir une cible.
- [Création de fournisseurs](features/provider-authoring.md) — étendre.
- [Dépannage](troubleshooting.md) — résoudre un échec.
