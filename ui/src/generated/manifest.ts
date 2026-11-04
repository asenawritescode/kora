// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: '664b242e734516f434d3f7ee3fbd4bfea954b388',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '66146a84f2da2cad7eed1c9255933a7843ac42880f11e4aed5ec8ee3ccf8872a',
  schemaSHA256: '785d164497ceab9a32ac6134b533624f1826a9d03da860619b27c955e047e882',
} as const
