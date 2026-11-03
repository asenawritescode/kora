// This manifest pins the Engine source revision and bytes used to generate
// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.
export const ENGINE_CONTRACT_MANIFEST = {
  schemaVersion: 2,
  sourceRevision: 'ab1dee1e8097712aa673f199d3ca29df686d8d52',
  sourceTree: 'clean',
  artifact: 'contract/generated/engine-v2.ts',
  artifactSHA256: '663c6077118e1276e3606c2cfcc7ce4b5adce655634a4762663dbeec67bbe09e',
  schemaSHA256: '7d30a1d23407009c3fcbfdae6e631b389dd6bef0007bbd1176fed7ea833c9190',
} as const
