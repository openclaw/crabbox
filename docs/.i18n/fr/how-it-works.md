<!-- source: how-it-works.md; sha256: 2e0288463ff74a878d06a6226e5924a2ce5426c54796c463626ae14219ab37b5 -->
# Fonctionnement de Crabbox {#how-crabbox-works}

Pour découvrir le modèle complet ou identifier le composant responsable avant de modifier le code.

## Résumé {#tl-dr}

Crabbox fournit des machines à la demande. Modifiez localement pendant les tests et compilations sur Linux, macOS, Windows natif ou WSL2. Dans une flotte partagée :

- **CLI** (`cmd/crabbox`, `internal/cli`) : réservation, synchronisation des changements, exécution, sortie et nettoyage.
- **Coordinateur** (`worker/src`) : identifiants, état, expiration, nettoyage, usage et coûts. Cloudflare Workers/Durable Object ou Node.js/PostgreSQL/pg-boss.

Les exécuteurs ne détiennent pas les secrets du courtier. Le dépôt gère préparation, dépendances et travail. Le coordinateur conserve historique, journaux et télémétrie ; téléchargez ou publiez captures et artefacts avant de libérer. Les fournisseurs directs peuvent fonctionner sans coordinateur.

## Composants {#the-pieces}

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

La CLI contacte le coordinateur en HTTPS puis l’exécuteur **directement** en SSH/rsync. L’exécuteur ne rappelle pas le coordinateur pour les commandes ordinaires ; ce chemin reste à sens unique. Le coordinateur gère les réservations, pas le transfert du dépôt ni l’exécution. La CLI envoie séparément métadonnées, aperçus, journaux et résultats ; les ponts interactifs passent aussi par lui. Le [service privé AWS](features/aws-private-workspaces.md) est une API SSM-only distincte sans plan de données SSH.

Les interactions longues peuvent utiliser le WebSocket authentifié `/v1/control` pour événements et battements de cœur, réduisant les requêtes à forte latence. HTTPS reste le stockage canonique et le repli compatible avec les anciennes versions.

## Responsabilités {#ownership}

| Couche | Responsable de |
| :--- | :--- |
| **CLI** | Configuration/options, clés SSH par réservation, préparation, Git/rsync, empreintes/cohérence, commande/sortie, WebSocket avec repli HTTP, libération |
| **Broker** | Authentification/identité, état sérialisé, secrets, création/suppression, expiration, inventaire, journaux/événements/télémétrie, usage/plafonds |
| **Provider** | Calcul brut : adaptateurs créant/supprimant Hetzner, AWS EC2, Azure VM, GCP et autres |
| **Runner** | Rien de durable : Linux avec SSH/Git/rsync/racine de travail, préparation spécifique ailleurs, hôtes SSH existants ; le dépôt installe les environnements du projet |

## Ce que fait `crabbox run` {#what-crabbox-run-does}

Cinq phases pour un parcours SSH intermédié. Les fournisseurs directs gèrent le cycle localement, les délégués gèrent le transport d’exécution.

**1. Planifier.** Priorité options → environnement → dépôt → utilisateur → défauts. Création d’un ID temporaire (`cbx_` + 12 chiffres hexadécimaux) et d’une clé dans `<user-config>/crabbox/testboxes/<lease>/id_ed25519` (RSA pour Windows AWS/Azure).

**2. Réserver.** `POST /v1/leases` transmet classe, fournisseur, cible, TTL, inactivité, slug, capacités et clé publique. Le coordinateur authentifie, sérialise, limite réservations et budget mensuel, récupère les prix, réserve le coût maximal TTL et provisionne avec replis région/marché. Il renvoie hôte, accès SSH, racine, expiration, ID final et slug. Un ID différent entraîne le renommage du dossier de clés local.

**3. Synchroniser.** Attendre SSH et la sonde, préparer Git depuis origin/référence de base. Des empreintes égales évitent le transfert. Sinon rsync POSIX (`/work/crabbox/<lease>/<repo>` sous Linux) ou archive tar de manifeste sous Windows natif, puis contrôles de cohérence et préparation de l’historique de base si disponible.

**4. Exécuter.** Battements en arrière-plan, commande SSH et flux stdout/stderr. En mode intermédié, événements par phase enregistrés au courtier.

**5. Libérer.** Les nouvelles réservations sont libérées sauf `--keep`, les réutilisations explicites `--id` sont conservées. `--stop-after` peut remplacer cette règle. Le fournisseur nettoie et libère le coût réservé. En cas d’échec avant SSH sur une machine nouvelle non conservée, `crabbox run` la remplace une fois. Pas de commande dupliquée sur une réservation conservée ou explicitement réutilisée.

## Machines prêtes et réutilisation {#warm-boxes-and-reuse}

`crabbox warmup` utilise la même création mais conserve la machine sans commande. Réutilisation explicite par slug ou ID :

```sh
crabbox warmup --profile project-check --slug blue-lobster
crabbox run --id blue-lobster -- pnpm test:changed
crabbox ssh --id blue-lobster
crabbox stop blue-lobster
```

Chaque réservation a un ID `cbx_…` et un slug. Les slugs automatiques incluent huit chiffres d’empreinte, comme `blue-lobster-1f3a9c2b`. `--slug` personnalise, `--id` accepte les deux. Les battements mettent à jour `lastTouchedAt` et l’expiration inactive. Le planificateur durable libère les réservations abandonnées.

## Fournisseur intermédié ou direct {#brokered-vs-direct-provider}

Le chemin **intermédié** est habituel :

```text
CLI -> coordinator (Cloudflare or Node/PostgreSQL) -> provider API
CLI -> runner over SSH/rsync
```

Pour partager l’infrastructure sans secrets locaux, avec nettoyage, usage et coûts centralisés. Actif pour `hetzner`, `aws`, `azure`, `daytona`, `gcp` seulement avec une URL via `CRABBOX_COORDINATOR` ou `config set-broker`.

Sans URL, le chemin est **direct** :

```text
CLI -> provider API
CLI -> runner over SSH/rsync
```

Des identifiants locaux sont requis : chaîne AWS SDK ou token Hetzner, par exemple. Pas d’historique central ni de battement au courtier. Utile pour diagnostiquer le courtier et obligatoire pour les adaptateurs directs comme bacs à sable, SSH statique et conteneurs locaux.

`broker.mode: registered` reste direct mais réplique métadonnées par propriétaire et battements. Il ajoute inventaire, partage optionnel et WebVNC sortant sans transférer secrets ni autorité de nettoyage. Libérer supprime seulement les métadonnées par défaut ; un adaptateur sortant explicitement lié peut supprimer après confirmation utilisateur. Les enregistrements ne comptent pas dans quotas, coûts, images, pools ou maintenance gérés.

SSH statique (`provider: ssh`) contourne toujours le courtier. macOS et WSL2 utilisent rsync POSIX, Windows natif PowerShell/tar.

## Authentification et identité {#auth-and-identity}

Les utilisateurs ont des tokens GitHub signés, l’automatisation fiable des bearer partagés. Cloudflare Access peut s’ajouter ; Node ne fait confiance aux en-têtes d’identité que depuis les CIDR de proxy configurés.

```text
Authorization: Bearer <token>
X-Crabbox-Owner: <email>
X-Crabbox-Org: <org>
```

Pour `crabbox login`, le propriétaire est `github:<numeric-id>`, immuable. En mode partagé : `CRABBOX_OWNER`, environnement d’email Git ou `git config user.email` ; `CRABBOX_ORG` définit l’organisation. Les en-têtes Access bruts sont ignorés ; seul l’email d’un JWT vérifié est accepté. Voir [Sécurité](security.md).

## Modèle de synchronisation {#sync-model}

Local d’abord, sans copie propre exigée :

- préparation Git depuis origin/base ;
- superposition rsync avec somme de contrôle optionnelle ;
- empreintes pour éviter les transferts inutiles ;
- exclusion des gros répertoires ;
- protection contre les suppressions massives suspectes ;
- historique de base pour tester les changements.

Humains et agents modifient localement et exécutent à distance. `crabbox sync-plan` donne un aperçu en lecture seule.

## Coûts et usage {#cost-and-usage}

Deux valeurs par réservation :

```text
estimatedUSD   elapsed runtime cost so far
reservedUSD    worst-case TTL cost reserved before provisioning
```

Priorité des tarifs horaires :

```text
1. CRABBOX_COST_RATES_JSON   explicit override
2. provider live pricing     e.g. EC2 spot history, Hetzner server-type prices
3. built-in fallback rates   last-resort defaults
```

Hetzner convertit en USD via `CRABBOX_EUR_TO_USD`. `CRABBOX_MAX_*` limite réservations et USD mensuels globalement et par propriétaire/organisation ; dépassement : HTTP 429. `crabbox usage` interroge `GET /v1/usage` par utilisateur, organisation, fournisseur et type.

## Échec et nettoyage {#failure-and-cleanup}

Arrêts, SSH perdu, cloud-init défaillant, succès partiel, reprises et machines survivant aux processus sont prévus :

- décisions sérialisées ;
- création idempotente si possible ;
- étiquettes Crabbox ;
- libération répétable sans danger ;
- alarmes Durable Object ou pg-boss avec réconciliation ;
- nettoyage direct conservateur ;
- nettoyage intermédié sous responsabilité du courtier.

Avec coordinateur, `crabbox cleanup` refuse de balayer les ressources : ce rôle appartient au coordinateur.

## Pour continuer {#where-to-go-next}

- [Architecture](architecture.md) : composants, API, état, échecs.
- [Orchestrateur](orchestrator.md) : responsabilités, cycle, coûts.
- [CLI](cli.md) : commandes, configuration, sortie, codes.
- [Fonctions](features/README.md) : par domaine.
- [Commandes](commands/README.md) : par commande.
- [Infrastructure](infrastructure.md) : environnements, entrée, secrets, DNS, configuration.
- [Sécurité](security.md) : authentification, SSH, nettoyage, confiance.
