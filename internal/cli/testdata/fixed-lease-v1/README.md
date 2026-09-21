These synthetic records were captured from the v1 writers at
`eaa0da7e779c725fc5dcd6aaa84c58435539b87e` using the existing Parallels, Tenki,
Proxmox, and Daytona provider fixtures through a Go source overlay. Repository
and Daytona work-root inputs were set to `/fixture/repo` and `/fixture/work`
before acquisition. No live provider or real credentials were used.

The acquired records here exercise native attempt preservation during journal
upgrade. Matching pre-engine released records in each provider's `testdata`
directory exercise that provider's terminal validator and single-use replay
fence without rewriting the old record.
