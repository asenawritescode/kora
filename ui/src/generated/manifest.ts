// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: 'b601d233e410ce66af9d8d24708faa181a907146',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '3a051c45a80f28bfbcc9b6fea28d3be2b12450e536e6d5d46cf3e3c353bf2ba5',
  schemaSHA256: 'c7ba49e0cb23aad0a1a231319378d4c56b5d83d86389f201dfc8f35e70871e62',
} as const
