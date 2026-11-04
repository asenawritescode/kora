#!/usr/bin/env node

// Seed one idempotent CRM stress-test dataset through the tenant API.
// Required env: KORA_API_BASE, KORA_COOKIE, KORA_CSRF.

const apiBase = (process.env.KORA_API_BASE || 'http://localhost:5173/s/crm/api/v1').replace(/\/$/, '')
const cookie = process.env.KORA_COOKIE
const csrf = process.env.KORA_CSRF

if (!cookie || !csrf) {
  console.error('Set KORA_COOKIE and KORA_CSRF to an authenticated tenant session.')
  process.exit(1)
}

const headers = {
  cookie,
  'X-Kora-CSRF-Token': csrf,
  'content-type': 'application/json',
}

async function kernelCommand(command, idempotencyKey, payload) {
  const response = await fetch(`${apiBase}/kernel/${command}`, {
    method: 'POST',
    headers: { ...headers, 'Idempotency-Key': idempotencyKey },
    body: JSON.stringify({ idempotency_key: idempotencyKey, payload }),
  })
  const result = await response.json()
  if (!response.ok) {
    throw new Error(`POST kernel/${command} (${response.status}): ${result.error?.message || 'request failed'}`)
  }
  return result.data
}

const sessionURL = apiBase.replace(/\/api\/v1$/, '/api/auth/me')
const sessionResponse = await fetch(sessionURL, { headers })
const sessionBody = await sessionResponse.json()
const currentUser = sessionBody.data
if (!sessionResponse.ok || !currentUser?.name || !(currentUser.roles || []).includes('Administrator')) {
  throw new Error('The current tenant session must be an authenticated Administrator.')
}

async function request(path, { method = 'GET', body } = {}) {
  const response = await fetch(`${apiBase}/${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const result = await response.json()
  if (!response.ok) {
    throw new Error(`${method} ${path} (${response.status}): ${result.error?.message || 'request failed'}`)
  }
  return result
}

async function api(path, options) {
  const result = await request(path, options)
  return result.data
}

async function existing(doctype, search) {
  const params = new URLSearchParams({ limit: '200', search })
  return await api(`resource/${encodeURIComponent(doctype)}?${params}`) || []
}

const companies = [
  ['Acacia Health Network', 'Healthcare'], ['Mwangaza Medical Group', 'Healthcare'],
  ['Baraka Community Clinics', 'Healthcare'], ['Lakeview Diagnostics', 'Healthcare'],
  ['Kifaru Farm Supply', 'Manufacturing'], ['Tawi Fresh Produce', 'Retail'],
  ['Pamoja Microfinance', 'Finance'], ['Northstar Distribution', 'Retail'],
  ['Imani Learning Group', 'Education'], ['Savanna Solar Works', 'Manufacturing'],
  ['Jirani Property Partners', 'Real Estate'], ['Maua Hospitality', 'Professional Services'],
  ['Kipepeo Logistics', 'Manufacturing'], ['Umoja Credit Union', 'Finance'],
  ['Rift Valley Foods', 'Retail'], ['NuruCare Pharmacy', 'Healthcare'],
  ['Mizani Accounting', 'Professional Services'], ['Bahari Marine Services', 'Manufacturing'],
  ['Tangaza Telecom', 'Technology'], ['Greenline Packaging', 'Manufacturing'],
  ['Soko Wholesale Co-op', 'Retail'], ['Malaika Children’s Trust', 'Non-Profit'],
  ['Afya Occupational Health', 'Healthcare'], ['Kaskazi Travel Group', 'Professional Services'],
  ['Kiboko Equipment Hire', 'Manufacturing'], ['Jua Data Systems', 'Technology'],
  ['Pioneer Technical College', 'Education'], ['Ziwa Water Services', 'Government'],
  ['Kijani Construction', 'Real Estate'],
]

const firstNames = ['Amina', 'Brian', 'Wanjiku', 'David', 'Njeri', 'Peter', 'Zawadi', 'Musa', 'Faith', 'Kevin']
const lastNames = ['Njoroge', 'Otieno', 'Kamau', 'Wekesa', 'Mwangi', 'Abdi', 'Kiptoo', 'Ochieng', 'Muli', 'Kariuki']
const slug = (value) => value.toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '')
const isoDate = (year, month, day) => `${year}-${String(month).padStart(2, '0')}-${String(day).padStart(2, '0')}`

const knownCompanies = await existing('Company', 'Pipeline Demo')
const companyByLabel = new Map(knownCompanies.map((company) => [company.company_name, company]))
const allCompanies = []
for (let index = 0; index < companies.length; index++) {
  const [baseName, industry] = companies[index]
  const companyName = `Pipeline Demo ${baseName}`
  let company = companyByLabel.get(companyName)
  if (!company) {
    const year = 2023 + (index % 4)
    company = await api('resource/Company', {
      method: 'POST',
      body: {
        company_name: companyName,
        website: `https://${slug(baseName)}.example`,
        industry,
        company_type: index < 12 ? 'Customer' : 'Prospect',
        employee_count: 8 + ((index * 47) % 740),
        annual_revenue: 250_000 + ((index * 317_500) % 18_000_000),
        first_engaged_date: isoDate(year, 1 + ((index * 3) % 12), 3 + ((index * 7) % 24)),
        notes: 'Synthetic CRM pipeline test account. Safe to remove after the demo.',
      },
    })
  }
  allCompanies.push(company)
}

const knownContacts = await existing('Contact', 'crm-demo-')
const contactByEmail = new Map(knownContacts.map((contact) => [contact.email, contact]))
const contacts = []
for (let index = 0; index < allCompanies.length; index++) {
  const company = allCompanies[index]
  const email = `crm-demo-${String(index + 1).padStart(2, '0')}@${slug(company.company_name.replace('Pipeline Demo ', ''))}.example`
  let contact = contactByEmail.get(email)
  if (!contact) {
    contact = await api('resource/Contact', {
      method: 'POST',
      body: {
        first_name: firstNames[index % firstNames.length],
        last_name: `${lastNames[index % lastNames.length]} Demo ${String(index + 1).padStart(2, '0')}`,
        email,
        company: company.name,
        job_title: index % 3 === 0 ? 'Managing Director' : index % 3 === 1 ? 'Operations Manager' : 'Finance Lead',
        contact_type: index < 12 ? 'Customer' : 'Prospect',
      },
    })
  }
  contacts.push(contact)
}

const knownDeals = await existing('Deal', 'Pipeline Demo FY')
const dealByLabel = new Map(knownDeals.map((deal) => [deal.deal_name, deal]))
const stateCounts = { Lead: 0, Qualified: 0, Proposal: 0, Negotiation: 0, 'Closed Won': 0, 'Closed Lost': 0 }
let dealsCreated = 0
let dealsReused = 0

for (let index = 0; index < 99; index++) {
  let outcome
  if (index < 18) outcome = 'Closed Won'
  else if (index < 32) outcome = 'Closed Lost'
  else if (index < 54) outcome = 'Qualified'
  else if (index < 69) outcome = 'Proposal'
  else if (index < 78) outcome = 'Negotiation'
  else outcome = 'Lead'

  const year = 2023 + (index % 4)
  const closeMonth = year === 2026 ? 1 + (index % 9) : 1 + ((index * 5) % 12)
  const expectedDate = isoDate(year, closeMonth, 3 + ((index * 7) % 24))
  const dealName = `Pipeline Demo FY${year} — ${companies[index % companies.length][0]} ${String(index + 1).padStart(3, '0')}`
  let deal = dealByLabel.get(dealName)

  if (!deal) {
    const companyIndex = index % allCompanies.length
    const company = allCompanies[companyIndex]
    const contact = contacts[companyIndex]
    const isClosed = outcome === 'Closed Won' || outcome === 'Closed Lost'
    let actualCloseDate
    if (isClosed) {
      const month = year === 2026 ? Math.min(10, closeMonth) : closeMonth
      const day = year === 2026 && month === 10 ? 10 : 5 + ((index * 3) % 20)
      actualCloseDate = isoDate(year, month, day)
    }
    const expectedCloseDate = isClosed ? expectedDate : (index % 7 === 0 ? isoDate(2025, 12, 12) : isoDate(2026, 11 + (index % 2), 5 + (index % 20)))

    deal = await api('resource/Deal', {
      method: 'POST',
      body: {
        deal_name: dealName,
        contact: contact.name,
        company: company.name,
        owner_user: currentUser.name,
        expected_close_date: expectedCloseDate,
        ...(actualCloseDate ? { actual_close_date: actualCloseDate } : {}),
        probability: outcome === 'Closed Won' ? 100 : outcome === 'Closed Lost' ? 0 : Math.min(90, 15 + ((index * 11) % 75)),
        lead_source: ['Website', 'Referral', 'Social Media', 'Event', 'Partner'][index % 5],
        next_step: outcome === 'Closed Won' || outcome === 'Closed Lost' ? 'Archive outcome and share handoff' : ['Book discovery call', 'Send proposal', 'Confirm budget owner', 'Schedule product review'][index % 4],
        next_step_due_date: isoDate(2026, 10 + (index % 2), 20 + (index % 10)),
        items: [{ product_name: ['CRM rollout', 'Operations platform', 'Annual service plan'][index % 3], quantity: 1 + (index % 4), unit_price: 4_500 + ((index * 1_375) % 42_000) }],
        discount: 0,
        ...(outcome === 'Closed Lost' ? { loss_reason: ['Budget deferred', 'Chose another provider', 'Project cancelled'][index % 3] } : {}),
        notes: 'Synthetic pipeline stress-test opportunity with historical business dates.',
      },
    })
    dealsCreated++
  } else {
    dealsReused++
  }

  const stage = deal.pipeline_stage || 'Lead'
  if (stage !== outcome && (outcome === 'Closed Lost' || ['Qualified', 'Proposal', 'Negotiation', 'Closed Won'].includes(outcome))) {
    const salesPath = [
      ['Lead', null], ['Qualified', 'Qualify'], ['Proposal', 'Create Proposal'],
      ['Negotiation', 'Start Negotiation'], ['Closed Won', 'Close Won'],
    ]
    let actions
    if (outcome === 'Closed Lost') {
      actions = ['Close Lost']
    } else {
      const currentIndex = salesPath.findIndex(([state]) => state === stage)
      const targetIndex = salesPath.findIndex(([state]) => state === outcome)
      if (currentIndex < 0 || targetIndex < currentIndex) {
        throw new Error(`Cannot safely move ${deal.name} from ${stage} to ${outcome}.`)
      }
      actions = salesPath.slice(currentIndex + 1, targetIndex + 1).map(([, action]) => action)
    }
    let current = stage
    for (const action of actions) {
      const identity = `crm-demo:${deal.name}:${action}`
      const idempotencyKey = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(identity))
        .then((digest) => [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, '0')).join(''))
      await kernelCommand('workflow.status_transition', idempotencyKey, {
        doctype: 'Deal', name: deal.name, action,
      })
      current = action === 'Qualify' ? 'Qualified'
        : action === 'Create Proposal' ? 'Proposal'
          : action === 'Start Negotiation' ? 'Negotiation'
            : action === 'Close Won' ? 'Closed Won'
              : action === 'Close Lost' ? 'Closed Lost' : current
    }
    const fresh = await api(`resource/Deal/${encodeURIComponent(deal.name)}`)
    deal = fresh
  }
  stateCounts[deal.pipeline_stage || outcome] = (stateCounts[deal.pipeline_stage || outcome] || 0) + 1
}

const totals = await Promise.all(['Company', 'Contact', 'Deal'].map(async (doctype) => {
  const result = await request(`resource/${doctype}?limit=1`)
  return [doctype, result.meta?.total ?? 0]
}))

console.log(JSON.stringify({
  companiesAdded: allCompanies.length - knownCompanies.length,
  contactsAdded: contacts.length - knownContacts.length,
  dealsAdded: dealsCreated,
  dealsReused,
  seededDealOutcomes: stateCounts,
  tenantRecordTotals: Object.fromEntries(totals),
}, null, 2))
