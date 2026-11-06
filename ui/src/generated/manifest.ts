// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: 'da3f8fd368c9b8679dea2f62edf23deca48554ad',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: 'd21c6e697bff26962353ca88ed85c480d80a0554ffdd5500d39606c8513220cc',
  schemaSHA256: '625b075c4fd886ff553a6b0adefd13d57d8d58ca1a2228ce7c410c05c1aafabd',
} as const
