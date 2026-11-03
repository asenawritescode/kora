// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: 'd19b7351d4ee4d55e42d606700b3d39c0b2e7aa9',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '7d193bc1ecdcd5e8c7fccb54c6b76d14c4079b58ff995ad46112935aab9b82b4',
} as const
