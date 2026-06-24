export interface Template {
  name: string
  description: string
  icon: string
  category: string
  features: { iconType: string; label: string }[]
}

export const TEMPLATES: Template[] = [
  {
    name: 'Kiosk POS',
    description: 'A streamlined point-of-sale system for rapid retail environments.',
    icon: 'Store',
    category: 'Retail',
    features: [
      { iconType: 'Database', label: 'Products, Orders, Inventory' },
      { iconType: 'Layout', label: 'Quick Checkout Form' },
      { iconType: 'RefreshCw', label: 'Daily Sales Reconciliation' },
    ],
  },
  {
    name: 'B2B CRM',
    description: 'Manage high-value sales pipelines and account relationships.',
    icon: 'Users',
    category: 'Operations',
    features: [
      { iconType: 'Database', label: 'Accounts, Deals, Contacts' },
      { iconType: 'Layout', label: 'Lead Qualification Form' },
      { iconType: 'RefreshCw', label: 'Stage Progression Logic' },
    ],
  },
  {
    name: 'Clinic Admin',
    description: 'Patient scheduling, electronic records, and billing infrastructure.',
    icon: 'HeartPulse',
    category: 'Operations',
    features: [
      { iconType: 'Database', label: 'Patients, Appointments, Docs' },
      { iconType: 'Layout', label: 'Patient Intake Flow' },
      { iconType: 'RefreshCw', label: 'Reminder Automation' },
    ],
  },
  {
    name: 'School Admin',
    description: 'Manage enrollments, grades, attendance, and faculty.',
    icon: 'GraduationCap',
    category: 'Education',
    features: [
      { iconType: 'Database', label: 'Students, Courses, Grades' },
      { iconType: 'Layout', label: 'Enrollment Application' },
      { iconType: 'RefreshCw', label: 'Term Rollover Script' },
    ],
  },
  {
    name: 'Property Mgmt',
    description: 'Lease tracking, maintenance requests, and rent collection.',
    icon: 'Building2',
    category: 'Operations',
    features: [
      { iconType: 'Database', label: 'Units, Tenants, Leases' },
      { iconType: 'Layout', label: 'Maintenance Ticket Form' },
      { iconType: 'RefreshCw', label: 'Invoice Generation' },
    ],
  },
  {
    name: 'SACCO Core',
    description: 'Savings and Credit Cooperative ledger and member management.',
    icon: 'Landmark',
    category: 'Finance',
    features: [
      { iconType: 'Database', label: 'Members, Loans, Deposits' },
      { iconType: 'Layout', label: 'Loan Application' },
      { iconType: 'RefreshCw', label: 'Dividend Calculation' },
    ],
  },
  {
    name: 'Logistics Fleet',
    description: 'Vehicle tracking, maintenance schedules, and dispatch.',
    icon: 'Truck',
    category: 'Operations',
    features: [
      { iconType: 'Database', label: 'Vehicles, Drivers, Routes' },
      { iconType: 'Layout', label: 'Dispatch Assignment' },
      { iconType: 'RefreshCw', label: 'Maintenance Alerts' },
    ],
  },
  {
    name: 'Event Ticketing',
    description: 'Manage events, ticket tiers, and attendee scanning.',
    icon: 'Calendar',
    category: 'Operations',
    features: [
      { iconType: 'Database', label: 'Events, Tickets, Attendees' },
      { iconType: 'Layout', label: 'Ticket Purchase Flow' },
      { iconType: 'RefreshCw', label: 'QR Validation Webhook' },
    ],
  },
]
