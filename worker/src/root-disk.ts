// Keep internal/rootdisk/defaults.go in sync.
export function defaultRootGB(machineClass: string): number {
  switch (machineClass) {
    case "tiny":
      return 40;
    case "small":
      return 80;
    case "standard":
    case "fast":
      return 150;
    case "large":
      return 250;
    default:
      return 400;
  }
}
