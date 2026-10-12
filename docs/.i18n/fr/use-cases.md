<!-- source: use-cases.md; sha256: 769c5e58a1b095392245d30f0377bf87b025fa28c3eeca64f9337340d43cd3d7 -->
# Cas d’usage {#use-cases}

Donnez à chaque agent de programmation un ordinateur à la demande pour ses tests et compilations. Les agents continuent à modifier leurs copies locales pendant que Crabbox synchronise les changements non commités, exécute les commandes à distance et renvoie sorties et codes de retour. Des réservations distinctes évitent la concurrence pour le CPU, la mémoire et les ports du portable.

Choisissez Linux, macOS, Windows natif ou WSL2 lorsque le comportement de la plateforme compte. Réutilisez une machine prête pour les exécutions répétées et ouvrez un bureau ou un éditeur web pour examiner le résultat. Le [guide bureau, VNC, navigateur, Code et portail](features/interactive-desktop-vnc.md) explique l’accès interactif et les transferts de contrôle.

Partez de la charge de travail, pas du fournisseur :

```sh
crabbox providers recommend
crabbox providers recommend fast-feedback
crabbox providers recommend agent-sandbox --json
```

Sans cas d’usage, `crabbox providers recommend` affiche tous les noms disponibles. Les recommandations utilisent les métadonnées de capacités du dépôt. Elles ne contactent pas les fournisseurs, ne vérifient ni identifiants ni quotas, ne mesurent pas le démarrage et ne certifient aucune frontière de sécurité.

## Accélérer les tests et compilations {#speed-up-test-and-build-loops}

À utiliser lorsqu’une commande est trop lente, consomme trop de mémoire ou perturbe le travail local.

```sh
crabbox providers recommend fast-feedback
crabbox providers recommend linux-vm
crabbox run --provider hetzner -- pnpm test
```

Préférez `fast-feedback` pour les environnements locaux, caches réutilisables et cycles courts. Préférez `linux-vm` pour déplacer l’exécution hors du poste ou obtenir une VM complète et la sémantique SSH.

## Donner un environnement jetable à un agent {#give-a-coding-agent-a-disposable-environment}

Utilisez une réservation SSH distante ou un bac à sable délégué pour exécuter les commandes hors du poste. Un bac à sable local sépare l’espace de travail, mais utilise toujours le portable. Crabbox gère l’exécution et le nettoyage ; l’agent appelant ou son harnais gère l’orchestration.

Créez une réservation indépendante depuis la copie de chaque agent :

```sh
crabbox warmup --provider hetzner --class standard --slug agent-tests
crabbox run --id agent-tests -- pnpm test
crabbox stop agent-tests
```

Utilisez un slug distinct par réservation. Les réservations prêtes conservent l’installation entre commandes ; vérifiez la configuration du dépôt et installez outils et dépendances avant les tests.

```sh
crabbox providers recommend agent-sandbox
crabbox providers recommend disposable-execution
crabbox providers recommend isolated-execution
```

Ces trois recommandations répondent à des besoins différents :

- `agent-sandbox` favorise les fournisseurs adaptés aux agents et aux environnements de développement ;
- `disposable-execution` exige un environnement temporaire nettoyable ;
- `isolated-execution` favorise les frontières de bacs à sable locaux et délégués.

La dernière est une aide au routage, pas une certification de sécurité. Crabbox suppose des opérateurs, configurations et charges fiables. Une machine jetable ne sécurise pas du code hostile. Lisez le modèle de confiance et de réseau avant de transmettre des secrets.

## Valider Linux, Windows, WSL2 ou macOS {#validate-linux-windows-wsl2-or-macos}

Utilisez les recommandations de plateforme lorsque le comportement dépend du système cible :

```sh
crabbox providers recommend linux-vm
crabbox providers recommend windows
crabbox providers recommend macos

crabbox run --provider aws --target windows --windows-mode wsl2 -- uname -a
```

Windows natif exécute PowerShell et les outils Windows. WSL2 applique le contrat POSIX dans une VM Windows compatible avec la virtualisation imbriquée. Les cibles macOS peuvent être des VM Apple Silicon locales ou des Mac cloud, selon le fournisseur.

## Tester un navigateur ou un bureau visible {#test-a-browser-or-visible-desktop}

Utilisez une réservation SSH avec bureau pour un navigateur, des captures, des entrées ou un transfert à un collègue :

```sh
crabbox providers recommend desktop
crabbox warmup --provider azure --target windows --windows-mode normal --desktop --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

WebVNC permet aux collègues autorisés d’observer et de prendre le contrôle. VNC natif utilise un tunnel SSH ; `crabbox code` ouvre code-server sur les réservations Linux compatibles. Le portail expose les réservations, l’état des ponts et les preuves conservées. Gardez le pont local actif pendant l’utilisation du navigateur. Les prérequis figurent dans le [guide d’accès combiné](features/interactive-desktop-vnc.md).

Ajoutez le routage des preuves si les résultats doivent survivre à la machine :

```sh
crabbox providers recommend run-evidence
crabbox providers recommend failure-diagnostics
```

## Réutiliser un état prêt {#reuse-warm-state}

Utilisez une machine prête quand répéter l’installation coûte plus que conserver ou restaurer l’état :

```sh
crabbox providers recommend warm-start
crabbox providers recommend versioned-workspace

crabbox warmup --slug warm-tests
crabbox run --id warm-tests -- pnpm test:changed
```

`warm-start` utilise des signaux comme l’environnement local, les volumes de cache, les sessions conservées, la pause/reprise et l’état du travail. Il ne promet pas la même primitive d’instantané ou de pool prêt chez tous les fournisseurs.

## Distribuer des expériences en parallèle {#fan-out-parallel-experiments}

Utilisez des espaces de travail avec fork pour des branches parallèles, plusieurs tentatives d’agent ou des fragments de tests partant d’un état préparé :

```sh
crabbox providers recommend fanout-testing
crabbox checkpoint fork <checkpoint-id> --count 4
```

Il s’agit d’un routage neutre des points de contrôle et forks, pas d’une garantie de fork de la mémoire vive d’une microVM.

## Exécuter des charges GPU {#run-gpu-workloads}

Utilisez la recommandation GPU pour tester des modèles, compiler CUDA, effectuer du rendu ou d’autres tâches accélérées :

```sh
crabbox providers recommend gpu
crabbox run --provider runpod -- nvidia-smi
```

Les fournisseurs GPU avec SSH conviennent généralement mieux au débogage interactif. Les fournisseurs délégués conviennent souvent mieux lorsque le fournisseur doit gérer l’exécution et collecter les résultats.

## Exécuter localement sans identifiants cloud {#run-locally-without-cloud-credentials}

Utilisez un conteneur, une VM ou un bac à sable à politiques local pour un test rapide sur le poste :

```sh
crabbox providers recommend local
crabbox providers recommend offline-validation
crabbox run --provider local-container -- pnpm test
```

Local ne signifie pas isolation identique. Les conteneurs compatibles Docker, Apple VM, Multipass VM, Windows Sandbox et les environnements à politiques ont des contrats différents d’accès à l’hôte, de persistance et de nettoyage. Lisez la page du fournisseur avant d’exécuter du code inconnu.

## Utiliser l’infrastructure existante {#use-infrastructure-you-already-own}

Crabbox peut utiliser un hôte SSH permanent, une infrastructure de virtualisation privée ou un compte cloud direct sans coordinateur partagé :

```sh
crabbox providers recommend byo-ssh
crabbox providers recommend self-hosted
crabbox providers recommend linux-vm
```

Utilisez `ssh` lorsque Crabbox ne doit pas gérer le cycle de vie de l’hôte. Utilisez Proxmox, XCP-ng, Incus, KubeVirt ou Firecracker lorsque Crabbox doit créer et nettoyer l’environnement.

## Partager une flotte d’équipe contrôlée {#share-a-governed-team-fleet}

Utilisez le coordinateur pour partager identifiants, propriété des réservations, nettoyage, historique, limites de réservations actives et plafonds mensuels :

```sh
crabbox providers recommend team-cloud
crabbox login --url https://broker.example.com
crabbox usage --scope user
```

Le coordinateur est un plan de contrôle. La CLI se connecte directement aux exécuteurs SSH ordinaires pour synchroniser et exécuter, tout en enregistrant les preuves auprès du coordinateur. `/portal` présente l’inventaire et les liens vers bureaux, éditeurs et détails. Enregistrer un fournisseur direct ajoute de la visibilité sans transférer ses identifiants ni la responsabilité du nettoyage.

## Besoin d’un chemin plus précis ? {#need-a-more-specific-route}

La CLI recommande aussi des fournisseurs pour les artefacts, preuves CI, interpréteurs, diagnostics, connexions MCP, confinement réseau, pause/reprise, URL d’aperçu, accessibilité réseau, développement distant, observabilité, sessions réutilisables, tests web et exécution Worker/modules.

Consultez [`crabbox providers recommend`](commands/providers.md#providers-recommend) pour la liste actuelle, les alias, filtres, scores et limites.

## Documentation associée {#related-docs}

- [Sélection des fournisseurs](features/provider-selection.md)
- [Référence des fournisseurs](providers/README.md)
- [Exécution imbriquée](features/nested-execution.md)
- [Tarifs et coûts](pricing.md)
