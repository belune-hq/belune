// Draft row identity is local to this page and never persisted. A counter
// stays unique without randomUUID, which is unavailable on plain HTTP installs.
let counter = 0;

export function newClientId(): string {
  counter += 1;
  return `local-${counter}`;
}
