// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: 'b7198bfce553ea5fed2533a9dfee289098d05a59',
  sourceTree: 'uncommitted',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '097ef41615b608b62c4fa26407c0a948788c47c86027d8af71de7c3227fc576a',
} as const
