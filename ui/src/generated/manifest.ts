// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: '8ed5eaa9d1130fa62813b6993a91887b538073a9',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '9d51a7b8ea68a748f939a14d36c700969791acfcfd7003a453732a0a25f59a9d',
  schemaSHA256: '8bb1dc611263213392114ee59e2e343d9ea9bd917953b7b544691b0a2c09c433',
} as const
