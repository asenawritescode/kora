// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: '8ae620a3450e7493fea3081d001f488fae3a8058',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: 'c91e359e85c60047a0d92bc6a8edd355d7f217a5b9a62e888374c0ce3c29dcfe',
  schemaSHA256: 'f4614d84fa3ec9ab9d6c8f7322ae003219197a11fec91de148dab66e6b49840d',
} as const
