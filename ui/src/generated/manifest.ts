// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: '91f836bfd04d5a494d23b16f4d5250ce95251e41',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '4fa507531d06a9b41f42e30811e3def3bdbd8c89da43d2f939f45df893a75aa4',
  schemaSHA256: '221e76c47692cca135246b7abfdfde8e3cbcd3a59b964bc5a1f80f45e1f4a683',
} as const
