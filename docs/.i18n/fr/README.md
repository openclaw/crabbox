<!-- source: README.md; sha256: 008297f255e89d26769a8517dd1c425b15462564563cce18859b56b7640a570e -->
# Crabbox {#crabbox}

**Des ordinateurs à la demande pour les agents.**

## Qu’est-ce que Crabbox ? {#what-crabbox-is}

Crabbox fournit aux agents et aux humains des ordinateurs pour les tests et compilations. Continuez à modifier votre copie locale pendant qu’une machine distante exécute l’arbre de travail, changements non commités compris. Crabbox diffuse la sortie, renvoie le code de retour et nettoie la réservation selon le contrat du fournisseur.

Une fois le fournisseur configuré, exécutez la commande habituelle du dépôt :

```sh
crabbox run -- pnpm test
```

## Un cycle, plusieurs types de machines {#one-loop-many-kinds-of-box}

Le cycle distant standard :

1. **Modifiez localement** la copie existante.
2. **Réservez une machine** ou réutilisez une capacité prête.
3. **Synchronisez** l’arbre, y compris les changements non commités.
4. **Exécutez** la commande sur la machine.
5. **Recevez la sortie et le code de retour**.
6. **Nettoyez** les ressources possédées ou conservez-les pour la suite.

Les fournisseurs délégués peuvent gérer exécution et synchronisation. Réutilisation, preuves et nettoyage suivent leurs capacités et la politique d’exécution.

L’infrastructure peut changer sans transformer le processus en script spécifique. Commencez par les [Cas d’usage](use-cases.md) si la tâche est connue, ou la [Référence fournisseurs](providers/README.md) si le lieu d’exécution est fixé.

## Plus d’agents, moins d’attente {#more-agents-less-waiting}

- **Agents en parallèle :** séparez tests et compilations pour éviter la concurrence pour CPU, RAM et ports.
- **Machines prêtes :** réutilisez des environnements préparés pour raccourcir les cycles.
- **Multiplateforme :** Linux, macOS, Windows natif ou WSL2 depuis la même copie, selon le fournisseur.
- **Voir et piloter :** [VNC ou WebVNC](features/interactive-desktop-vnc.md), navigateur, ou VS Code web via `--code` sur Linux géré. Le [portail](features/portal.md) authentifié affiche réservations et journaux ; WebVNC permet d’observer ou de contrôler pendant que le pont local fonctionne.
- **Capacité partagée :** un coordinateur optionnel conserve les identifiants, partage les réservations et applique expiration et plafonds aux fournisseurs intermédiés.

Le [catalogue](providers/README.md) couvre clouds, VM locales, hôtes SSH et bacs à sable délégués. Le [contrat de plugin externe](providers/external.md) connecte votre backend. La liste et les comptes de catégories du site sont générés depuis les métadonnées.

## Commencer ici {#start-here}

- [Premiers pas](getting-started.md) — installation et première exécution distante.
- [Cas d’usage](use-cases.md) — retour rapide, agents, validation système, QA web, parallélisme et GPU.
- [Tarifs et coûts](pricing.md) — logiciel libre, calcul fourni par vos soins et limites.
- [Fonctionnement de Crabbox](how-it-works.md) — CLI, coordinateur et exécuteur.
- [Vision de Crabbox](../VISION.md) — périmètre, exclusions et sécurité du cycle de vie.

## Choisir un modèle d’exploitation {#pick-an-operating-model}

| Chemin | Usage idéal | Propriété |
| --- | --- | --- |
| Environnement local | Vérifications rapides sans identifiants | Votre poste et son environnement |
| Cloud direct ou SSH | Comptes personnels, hôtes privés, virtualisation interne | Votre compte ou infrastructure |
| Coordinateur d’équipe | Identifiants, réservations, nettoyage, usage et plafonds partagés | Votre Cloudflare ou Node.js/PostgreSQL |
| Exécution déléguée | Agents, CI, navigateur ou GPU gérés par le fournisseur | Fournisseur ou opérateur choisi |

Crabbox est sous licence MIT. Il n’existe actuellement pas d’offre de plan de contrôle hébergé ; calcul et infrastructure du coordinateur sont facturés par leurs opérateurs. Voir [Tarifs et coûts](pricing.md).

## Frontière de confiance {#trust-boundary}

Exécutez des dépôts fiables avec des collègues fiables. L’isolation dépend de l’environnement. Réutilisation et nettoyage destructif exigent une propriété vérifiée de la ressource et de la déclaration exactes. Toute ambiguïté bloque l’opération.

Utilisez la [Sélection des fournisseurs](features/provider-selection.md), puis lisez le fournisseur et le [modèle de sécurité](security.md). Une recommandation de parcours n’est pas une certification.

## Approfondir {#go-deeper}

- **CLI et configuration :** [CLI](cli.md), [Commandes](commands/README.md), [Configuration](features/configuration.md), [Intégration du dépôt](features/repository-onboarding.md).
- **Flotte et exploitation :** [Architecture](architecture.md), [Infrastructure](infrastructure.md), [Exploitation](operations.md), [Observabilité](observability.md), [Appairage en lecture seule](features/device-pairing.md), [Sécurité](security.md).
- **Exécution et preuves :** [Tâches](features/jobs.md), [Préparation Actions](features/actions-hydration.md), [Artefacts](features/artifacts.md), [Points de contrôle](features/checkpoints.md), [Bureau interactif et VNC](features/interactive-desktop-vnc.md).
- **Frontières des plateformes :** [Exécution imbriquée](features/nested-execution.md) distingue WSL2, conteneurs, hôtes KVM préparés et bacs à sable locaux.
- **Extension :** [Intégrations](integrations/README.md), [Création de fournisseurs](features/provider-authoring.md), [Carte du code](source-map.md).

## À propos de ces documents {#about-these-docs}

Le Markdown de `docs/` alimente [crabbox.sh](https://crabbox.sh/). Le code fait foi ; la [Carte du code](source-map.md) relie le comportement à son implémentation.

Construisez et validez le site localement :

```sh
scripts/check-docs.sh
open dist/docs-site/index.html
```

Dans `docs/`, utilisez des titres de niveaux 1–4 et des blocs avec trois accents graves. Les liens sont contrôlés avec les identifiants du moteur ; exemples et commentaires ne réservent pas d’ancres. Conservez les ID publiés. Le Markdown réservé au dépôt garde ses règles d’ancres orientées GitHub, sans prétendre à une compatibilité Markdown GitHub complète.

Voir [Traduire la documentation](translations.md) pour ajouter ou actualiser une langue.
