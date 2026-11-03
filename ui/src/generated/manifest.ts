// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: '81f3da384c2ea60d3b8fd87549a94a3720292bc3',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '7366b179eafd72929973b3e7ba8760a996b06bdae9af14fae959924e7d9b2414',
  schemaSHA256: 'f57c1a69fbbb834f9bbd69a5d04dd169afdde2cc19ba5f01ed07e4f5d70c6e08',
} as const
