// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: '367e8d67ad1753bc95a9a6fa02947e85b7c8f2bf',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '56558265952226eac669175ed6a2cee4c1f99329ac401fa1c45c86a59f8efeed',
  schemaSHA256: '6c684ca8d69e9adec2a5564626cc56266ea902fa1c15ce4f97b0164a74e98da7',
} as const
