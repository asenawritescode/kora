import { createHash } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'

const artifactPath = fileURLToPath(new URL('../src/generated/engine-v2.ts', import.meta.url))
const manifestPath = fileURLToPath(new URL('../src/generated/manifest.ts', import.meta.url))
const artifact = await readFile(artifactPath)
const manifest = await readFile(manifestPath, 'utf8')
const pinned = manifest.match(/artifactSHA256:\s*'([0-9a-f]{64})'/)?.[1]
if (!pinned) throw new Error('Engine contract manifest does not pin an artifact SHA-256')
const digest = createHash('sha256').update(artifact).digest('hex')
if (digest !== pinned) throw new Error(`Engine v2 contract drift: got ${digest}, pinned ${pinned}`)
if (process.env.KORA_ENGINE_CONTRACT_PATH) {
  const engineArtifact = await readFile(process.env.KORA_ENGINE_CONTRACT_PATH)
  const engineDigest = createHash('sha256').update(engineArtifact).digest('hex')
  if (engineDigest !== digest) throw new Error(`Embedded UI contract differs from Engine: ${digest} != ${engineDigest}`)
}
console.log(`Engine v2 contract bundle verified (${digest})`)
