// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: '0db87539f0fab3a141169d56d79186420ae2c803',
  sourceTree: 'uncommitted',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: 'c702fbbdc1672814069b8742175b904dc5e843a1c141a2d945671a04d1442bf8',
} as const
