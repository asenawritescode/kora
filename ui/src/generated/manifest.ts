// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: '06816e89ffcddc266402e11d7e5dac0af171f6ec',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '097ef41615b608b62c4fa26407c0a948788c47c86027d8af71de7c3227fc576a',
} as const
