// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: '42371f89f98b55972d212773fc9913e636ff9fe1',
  sourceTree: 'uncommitted',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: 'aad2b22420f0785fb1d3e61febaa7bc958ffafddff3649bc588243b98e78fb84',
} as const
