// formatModel turns a sysObjectID into a human-readable model label.
// Two-layer strategy: (1) longest-prefix match against a curated table
// of common product families per vendor; (2) structural fallback that
// names the vendor + first few tokens of the product subtree. gosnmp
// returns OID strings with a leading dot — we normalize that off
// before matching, which the prior version of this helper missed.

type ProductPrefix = { prefix: string; label: string }

type VendorEntry = {
  enterprise: string
  name: string
  products: ProductPrefix[]
}

const VENDORS: VendorEntry[] = [
  {
    enterprise: '1.3.6.1.4.1.9',
    name: 'Cisco',
    products: [
      { prefix: '1.3.6.1.4.1.9.1.516', label: 'Catalyst 3550' },
      { prefix: '1.3.6.1.4.1.9.1.696', label: 'Catalyst 3750' },
      { prefix: '1.3.6.1.4.1.9.1.1208', label: 'Catalyst 3850' },
      { prefix: '1.3.6.1.4.1.9.1.2557', label: 'Catalyst 9300' },
      { prefix: '1.3.6.1.4.1.9.1.2693', label: 'Nexus 9300' },
      { prefix: '1.3.6.1.4.1.9.1.1216', label: 'Nexus 7000' },
      { prefix: '1.3.6.1.4.1.9.1.1166', label: 'Nexus 5000' },
      { prefix: '1.3.6.1.4.1.9.1.1862', label: 'ASR 9000' },
      { prefix: '1.3.6.1.4.1.9.1.1864', label: 'ISR 4000' },
    ],
  },
  {
    enterprise: '1.3.6.1.4.1.30065',
    name: 'Arista',
    products: [
      { prefix: '1.3.6.1.4.1.30065.1.3011.7050', label: '7050 series' },
      { prefix: '1.3.6.1.4.1.30065.1.3011.7060', label: '7060 series' },
      { prefix: '1.3.6.1.4.1.30065.1.3011.7150', label: '7150 series' },
      { prefix: '1.3.6.1.4.1.30065.1.3011.7170', label: '7170 series' },
      { prefix: '1.3.6.1.4.1.30065.1.3011.7250', label: '7250 series' },
      { prefix: '1.3.6.1.4.1.30065.1.3011.7280', label: '7280 series' },
      { prefix: '1.3.6.1.4.1.30065.1.3011.7300', label: '7300 series' },
      { prefix: '1.3.6.1.4.1.30065.1.3011.7320', label: '7320 series' },
      { prefix: '1.3.6.1.4.1.30065.1.3011.7368', label: '7368 series' },
      { prefix: '1.3.6.1.4.1.30065.1.3011.7500', label: '7500 series' },
      { prefix: '1.3.6.1.4.1.30065.1.3011.7800', label: '7800 series' },
    ],
  },
  {
    enterprise: '1.3.6.1.4.1.2636',
    name: 'Juniper',
    products: [
      { prefix: '1.3.6.1.4.1.2636.1.1.1.2.21', label: 'EX series' },
      { prefix: '1.3.6.1.4.1.2636.1.1.1.2.29', label: 'MX series' },
      { prefix: '1.3.6.1.4.1.2636.1.1.1.2.31', label: 'QFX series' },
      { prefix: '1.3.6.1.4.1.2636.1.1.1.2.55', label: 'SRX series' },
      { prefix: '1.3.6.1.4.1.2636.1.1.1.2.57', label: 'PTX series' },
      { prefix: '1.3.6.1.4.1.2636.1.1.1.2.130', label: 'ACX series' },
    ],
  },
  { enterprise: '1.3.6.1.4.1.674', name: 'Dell', products: [] },
  { enterprise: '1.3.6.1.4.1.6027', name: 'Force10/Dell', products: [] },
  { enterprise: '1.3.6.1.4.1.4526', name: 'Netgear', products: [] },
  { enterprise: '1.3.6.1.4.1.890', name: 'Zyxel', products: [] },
  { enterprise: '1.3.6.1.4.1.14988', name: 'MikroTik', products: [] },
  { enterprise: '1.3.6.1.4.1.11', name: 'HP/Aruba', products: [] },
  { enterprise: '1.3.6.1.4.1.25506', name: 'H3C', products: [] },
  { enterprise: '1.3.6.1.4.1.2011', name: 'Huawei', products: [] },
  { enterprise: '1.3.6.1.4.1.12356', name: 'Fortinet', products: [] },
  { enterprise: '1.3.6.1.4.1.25461', name: 'Palo Alto', products: [] },
  { enterprise: '1.3.6.1.4.1.8741', name: 'Extreme', products: [] },
  { enterprise: '1.3.6.1.4.1.6486', name: 'Alcatel-Lucent', products: [] },
]

function normalize(oid: string): string {
  if (!oid) return ''
  return oid.startsWith('.') ? oid.slice(1) : oid
}

function findVendor(oid: string): VendorEntry | undefined {
  return VENDORS.find(
    (v) => oid === v.enterprise || oid.startsWith(v.enterprise + '.'),
  )
}

function findProduct(oid: string, v: VendorEntry): ProductPrefix | undefined {
  return v.products
    .filter((p) => oid === p.prefix || oid.startsWith(p.prefix + '.'))
    .sort((a, b) => b.prefix.length - a.prefix.length)[0]
}

export function formatModel(oid: string): string {
  const o = normalize(oid)
  if (!o) return '—'
  const v = findVendor(o)
  if (!v) return oid
  const product = findProduct(o, v)
  if (product) return `${v.name} ${product.label}`
  const tail = o.slice(v.enterprise.length + 1)
  if (!tail) return v.name
  // Show the first 2–3 tokens after the enterprise OID; the full
  // string is preserved as the title attribute by the consumer.
  const tailShort = tail.split('.').slice(0, 3).join('.')
  return `${v.name} · ${tailShort}`
}
