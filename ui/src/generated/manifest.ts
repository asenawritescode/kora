// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: '97b909c348792355e0a0691029d8ac2d3cfa36e1',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '9fcb6c6f909579d1dcf6b69dd38665b94a848a5f098cba37f818e5489feda87b',
  schemaSHA256: '7f443c7388b23e014aee44b6b6aecc83724bf0cac26873ef1444d5ac00da7a0c',
} as const
