import type { Schemas } from '../core/api';

/** The posting catalog's areas (internal/catalog/labels.go), as members read them. */
export const AREA_LABELS: Record<string, string> = {
  engineering_software: 'Software engineering',
  engineering_other: 'Engineering (non-software)',
  it: 'IT & support',
  data_analytics: 'Data & analytics',
  product_design: 'Product & design',
  finance_accounting: 'Finance & accounting',
  sales: 'Sales',
  marketing_comms: 'Marketing & communications',
  customer_support: 'Customer support & success',
  operations_supply: 'Operations & supply chain',
  hr: 'People & HR',
  legal_compliance: 'Legal & compliance',
  healthcare: 'Healthcare',
  education: 'Education',
  research_science: 'Research & science',
  admin: 'Administration',
  consulting_pm: 'Consulting & project management',
  skilled_trades: 'Skilled trades',
  hospitality_retail: 'Hospitality & retail',
  media_arts: 'Media & arts',
  ai_training: 'AI training',
};

export const LEVEL_LABELS: Record<Schemas['SeniorityLevel'], string> = {
  intern: 'Intern',
  entry: 'Entry',
  mid: 'Mid',
  senior: 'Senior',
  staff: 'Staff',
  manager: 'Manager',
};

export const EMPLOYMENT_LABELS: Record<Schemas['PostingEmploymentType'], string> = {
  full_time: 'Full-time',
  contract: 'Contract',
  internship: 'Internship',
  part_time: 'Part-time',
};

/** ISO 3166-1 alpha-2 codes the server accepts (internal/catalog/country.go). */
export const COUNTRY_CODES =
  'AD AE AF AG AL AM AO AR AT AU AZ BA BB BD BE BF BG BH BI BJ BN BO BR BS BT BW BY BZ CA CD CF CG CH CI CL CM CN CO CR CU CV CY CZ DE DJ DK DM DO DZ EC EE EG ER ES ET FI FJ FR GA GB GD GE GH GM GN GQ GR GT GW GY HK HN HR HT HU ID IE IL IN IQ IR IS IT JM JO JP KE KG KH KR KW KZ LA LB LK LR LS LT LU LV LY MA MC MD ME MG MK ML MM MN MO MR MT MU MV MW MX MY MZ NA NE NG NI NL NO NP NZ OM PA PE PG PH PK PL PR PS PT PY QA RO RS RU RW SA SC SD SE SG SI SK SL SN SO SR SV SY SZ TD TG TH TJ TL TM TN TR TT TW TZ UA UG US UY UZ VE VN YE ZA ZM ZW'.split(
    ' ',
  );

const regions = new Intl.DisplayNames(['en'], { type: 'region' });
export function countryName(code: string): string {
  return regions.of(code) ?? code;
}
