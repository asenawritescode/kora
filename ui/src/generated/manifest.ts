// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: '14184ea2875acffc443a1e68b650163947982539',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '1d0fc0cdb31433db902591ccb2c2a1e251da88be898708e40e164dce6670be3f',
} as const
