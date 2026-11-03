// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: '5eb230dc46913acc42e717e0bc9e45a25561bf86',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: 'fa66769c67be115056dd1978a50b6e4d050936804201e069d26307080d66daf0',
  schemaSHA256: '562f057ab414990f364e4f775a185d13eea74365adb554e82d40f328029cf27c',
} as const
