// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: 'b75c9cef03defab38ef14d8b74f25b3fa7270b87',
  sourceTree: 'uncommitted',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '31195fce026f8b3eeef77a1ce223675fc4056bffbebbd78cd0e25c7c38e34936',
} as const
