export interface Article {
  category: string
  title: string
  description: string
  readTime: string
}

export const ARTICLES: Article[] = [
  {
    category: 'DevOps',
    title: 'Why YAML matters',
    description: 'Exploring the syntax, the pitfalls, and why declarative configuration remains the cornerstone of modern deployment pipelines.',
    readTime: '5 min read',
  },
  {
    category: 'Edge Computing',
    title: 'Deploying on Raspberry Pi',
    description: 'A practical guide to utilizing low-power ARM architecture for high-availability microservices at the edge of the network.',
    readTime: '8 min read',
  },
  {
    category: 'Architecture',
    title: 'Multi-tenancy at Scale',
    description: 'Database isolation strategies, routing mechanisms, and ensuring absolute data integrity across thousands of isolated organizational units.',
    readTime: '12 min read',
  },
]
