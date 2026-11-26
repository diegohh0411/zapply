package catalog

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/diegohh0411/zapply/internal/model/extraction"
	"github.com/diegohh0411/zapply/internal/model/systemone"
)

// LabelVersion is the version of the questions and fields below. Changing
// what they ask changes it, so labels can be told apart by what produced them.
const LabelVersion = 1

// Label values.
var (
	Seniorities     = []string{"intern", "entry", "mid", "senior", "staff", "manager"}
	EmploymentTypes = []string{"full_time", "contract", "internship", "part_time"}
	Areas           = []string{"engineering_software", "engineering_other", "it", "data_analytics", "product_design", "finance_accounting",
		"sales", "marketing_comms", "customer_support", "operations_supply", "hr", "legal_compliance", "healthcare", "education",
		"research_science", "admin", "consulting_pm", "skilled_trades", "hospitality_retail", "media_arts", "ai_training", "other"}
)

// Labels are what the classifier read off a posting. A nil field is unknown.
type Labels struct {
	Seniority      *string
	MinYears       *int
	Areas          []string
	Specialty      *string
	Countries      []string
	Remote         *bool
	EmploymentType *string
	Languages      []string
	// Confidence is the model's own certainty per field, 0 to 1.
	Confidence map[string]float64
}

// secondaryArea is how probable the runner-up area must be to count as the
// posting's second area. A decision answers one choice with a probability
// for every option, so a posting that is two things splits its probability.
const secondaryArea = 0.2

// areaCriteria describes each area for a question that picks one. A member's
// preferences ask it too, so both read the areas the same way.
var areaCriteria = map[string]string{
	"engineering_software": "Builds software: software, backend, frontend, full-stack, mobile, DevOps, SRE, platform, ML or data engineering, QA automation. A Solutions Engineer, Sales Engineer or Forward Deployed Engineer is engineering_software first and sales second.",
	"engineering_other":    "Engineering outside software: mechanical, electrical, civil, chemical, industrial, manufacturing, hardware, aerospace, process or quality engineering (Ingeniero de procesos, Ingeniero de calidad).",
	"it":                   "Runs and supports technology: IT support, help desk, systems or network administration, security operations, IT infrastructure, ERP administration (Soporte técnico, Sistemas).",
	"data_analytics":       "Analyzes data: data analyst, BI, analytics, data scientist, statistician, reporting, insights.",
	"product_design":       "Shapes products: product manager, product owner, UX, UI, product or graphic design for products, user research.",
	"finance_accounting":   "Money: accounting, finance, FP&A, audit, tax, treasury, payroll, billing, banking, investment (Contador, Contabilidad, Finanzas, Auditor).",
	"sales":                "Sells: account executive, business development, SDR, BDR, account manager, inside sales, partnerships with quotas (Ventas, Ejecutivo de ventas, Asesor comercial).",
	"marketing_comms":      "Markets and communicates: marketing, growth, SEO, content, social media, brand, PR, communications (Mercadotecnia, Comunicación).",
	"customer_support":     "Supports or retains customers: customer support, customer success, call center, client service (Atención a clientes, Servicio al cliente).",
	"operations_supply":    "Runs operations: operations, logistics, supply chain, procurement, purchasing, warehouse, fleet, inventory, planning (Logística, Compras, Almacén).",
	"hr":                   "People: HR, recruiting, talent acquisition, people operations, learning and development, compensation (Recursos Humanos, Reclutamiento).",
	"legal_compliance":     "Law and rules: lawyer, paralegal, legal counsel, compliance, regulatory, privacy, risk (Abogado, Jurídico, Cumplimiento).",
	"healthcare":           "Cares for health: doctors, nurses, pharmacists, therapists, dentists, medical technicians, clinical staff (Médico, Enfermera, Químico farmacéutico).",
	"education":            "Teaches: teachers, professors, tutors, instructional designers, school staff (Maestro, Profesor, Docente).",
	"research_science":     "Researches: scientists and researchers in labs or academia, R&D outside software, clinical research.",
	"admin":                "Administrative and office support: assistants, executive assistants, receptionists, office managers, data entry (Asistente administrativo, Recepcionista).",
	"consulting_pm":        "Consults or manages projects and programs across areas: consultants, project managers, program managers, PMO, business analysts, strategy.",
	"skilled_trades":       "Hands-on trades and production: electricians, technicians, mechanics, welders, drivers, machine operators, construction, maintenance (Técnico, Operador, Chofer, Mantenimiento).",
	"hospitality_retail":   "Serves in person: retail, stores, restaurants, hotels, tourism, cashiers, baristas, cooks (Cajero, Vendedor de piso, Mesero, Cocinero).",
	"media_arts":           "Creates media and art: writers, editors, journalists, video, photography, illustration, animation, music, game art (Redactor, Editor).",
	"ai_training":          "Trains or evaluates AI models as freelance or gig work: AI trainer, AI tutor, data annotator, RLHF writer or rater, expert contributor for platforms such as Mercor, Alignerr, Outlier, Scale AI, Remotasks or DataAnnotation.",
	"other":                "None of the other areas fits.",
}

// AreaCriteria is the description of every area, keyed by area.
func AreaCriteria() map[string]string { return areaCriteria }

// The decision asks these, in this order: q1 is seniority, and so on.
const (
	qSeniority = iota
	qArea
	qEmployment
	qRemote
	qMinYears
)

// minYearsOptions are the minimum-years buckets: each is the lower bound.
var minYearsOptions = map[string]string{
	"unstated": "The posting asks for no particular amount of experience, or does not say.",
	"0":        "No prior experience needed: new graduates, students or career starters are welcome.",
	"1":        "About one year of experience.",
	"2":        "Two years of experience.",
	"3":        "Three years of experience.",
	"4":        "Four years of experience.",
	"5":        "Five years of experience.",
	"6":        "Six or seven years of experience.",
	"8":        "Eight or nine years of experience.",
	"10":       "Ten or more years of experience.",
}

// decisionQuestions are the classifier's questions about `posting`. The
// posting may be in English, Spanish or both.
func decisionQuestions() []systemone.Question {
	ask := func(q string) json.RawMessage {
		return systemone.Raw(map[string]any{"question": q, "note": "The posting may be written in English or Spanish. Judge only what `posting` says."})
	}
	return []systemone.Question{
		qSeniority: {Type: "choice", Instructions: ask("What seniority is the role in `posting`? `posting.board_seniority`, when present, is the job board's own guess: use it as a hint, not as the answer, and prefer what the title and description say."),
			Criteria: systemone.Raw(map[string]string{
				"intern":  "An internship, trainee or student placement: Intern, Internship, Trainee, Working Student, Practicante, Prácticas, Becario, Pasante, Residente profesional.",
				"entry":   "Entry level or junior: Junior, Jr, Jr., Graduate, New Grad, Associate, Analyst I, Engineer I, Recién egresado, Egresado, Recién graduado, Sin experiencia, Auxiliar, Ayudante; about zero to two years of experience.",
				"mid":     "Mid level: an individual contributor with a few years of experience and no junior or senior marker: Engineer II, Semi Senior, Semi-Sr, SSr, Analista, Especialista; about two to five years.",
				"senior":  "Senior individual contributor: Senior, Sr, Sr., Engineer III, Especialista Senior; about five or more years.",
				"staff":   "An individual contributor above senior: Staff, Principal, Distinguished, Fellow, Architect, Arquitecto, Lead or Tech Lead without people to manage.",
				"manager": "Manages people, a team or a function: Manager, Gerente, Jefe, Jefa, Head of, Director, Directora, Coordinador of a team, Supervisor, VP, Vice President, Chief, C-level.",
			})},
		qArea: {Type: "choice", Instructions: ask("Which professional area is the role in `posting` mainly in? A role that spans two areas belongs to the one it is first, as the boundary cases in the options say."),
			Criteria: systemone.Raw(areaCriteria)},
		qEmployment: {Type: "choice", Instructions: ask("What kind of employment does `posting` offer?"),
			Criteria: systemone.Raw(map[string]string{
				"full_time":  "A full-time position, permanent or with no other arrangement stated (Tiempo completo, Indeterminado, Planta).",
				"contract":   "A contract, freelance, temporary or per-project engagement, including hourly gig work (Por proyecto, Temporal, Honorarios, Freelance).",
				"internship": "An internship, traineeship or student program (Prácticas, Becario, Servicio social).",
				"part_time":  "A part-time position (Medio tiempo, Tiempo parcial).",
			})},
		qRemote: {Type: "noul", Instructions: ask("Can the role in `posting` be done fully remotely, from home, with no regular office days? `posting.board_remote` is the job board's flag, a hint. Hybrid roles are not remote.")},
		qMinYears: {Type: "choice", Instructions: ask("How many years of work experience does `posting` ask for at least? Choose the largest option not above the minimum it states."),
			Criteria: systemone.Raw(minYearsOptions)},
	}
}

// postingState is what the decision reads.
func postingState(p postingRow) json.RawMessage {
	posting := map[string]any{"title": p.Title, "company": p.Company, "location": p.Location, "board_remote": p.Remote}
	if p.Description != "" {
		posting["description"] = clip(p.Description, maxDescription)
	}
	if p.SeniorityHint != "" {
		posting["board_seniority"] = p.SeniorityHint
	}
	return systemone.Raw(map[string]any{"posting": posting})
}

// maxDescription bounds the description sent to a model: the labels come
// from a posting's first pages, and longer text only costs more.
const maxDescription = 8000

// extractionState is what the extraction reads: the posting as text.
func extractionState(p postingRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Title: %s\nCompany: %s\nLocation: %s\n", p.Title, p.Company, p.Location)
	if p.Remote {
		b.WriteString("The job board marks it remote.\n")
	}
	if p.Description != "" {
		b.WriteString("\n" + clip(p.Description, maxDescription) + "\n")
	}
	return b.String()
}

// extractionRequest asks for the labels a decision cannot answer.
func extractionRequest(model string, p postingRow) extraction.Request {
	return extraction.Request{Model: model, State: extractionState(p), Fields: map[string]extraction.Field{
		"specialty": {Type: extraction.Text, Instructions: "The role's specialty within its field, in two to five English words, such as backend payments, pediatric nursing, B2B SaaS sales or tax accounting"},
		"countries": {Type: extraction.List, Items: extraction.Text, Instructions: "Every country where the job is based or that candidates may work from, as ISO 3166-1 alpha-2 codes such as US, MX or ES. " +
			"For a remote job limited to some countries or a region, list those countries. Leave it empty for a remote job open anywhere"},
		"languages": {Type: extraction.List, Items: extraction.Text, Instructions: "The languages the job requires candidates to work in, as ISO 639-1 codes such as en, es or pt, including the language the posting is written in"},
	}}
}

// decisionLabels reads a decision's answers, keyed q1, q2, … in the order of
// decisionQuestions. It reports false when the answers lack an area or a
// seniority, which every posting has.
func decisionLabels(answers map[string]systemone.Answer) (Labels, bool) {
	l := Labels{Confidence: map[string]float64{}}
	get := func(i int) (systemone.Answer, bool) {
		a, ok := answers[fmt.Sprintf("q%d", i+1)]
		return a, ok
	}
	conf := func(a systemone.Answer) float64 {
		if a.Confidence != nil {
			return round(*a.Confidence)
		}
		return round(a.Probabilities[a.Choice])
	}
	if a, ok := get(qSeniority); ok && contains(Seniorities, a.Choice) {
		l.Seniority = &a.Choice
		l.Confidence["seniority"] = conf(a)
	}
	if a, ok := get(qArea); ok && contains(Areas, a.Choice) {
		l.Areas = []string{a.Choice}
		if second, p := runnerUp(a); second != "" && second != "other" && a.Choice != "other" && p >= secondaryArea {
			l.Areas = append(l.Areas, second)
		}
		l.Confidence["areas"] = conf(a)
	}
	if a, ok := get(qEmployment); ok && contains(EmploymentTypes, a.Choice) {
		l.EmploymentType = &a.Choice
		l.Confidence["employment_type"] = conf(a)
	}
	if a, ok := get(qRemote); ok && a.Noul != nil {
		remote := *a.Noul >= 0.5
		l.Remote = &remote
		l.Confidence["remote"] = round(math.Max(*a.Noul, 1-*a.Noul))
	}
	if a, ok := get(qMinYears); ok {
		if n, err := strconv.Atoi(a.Choice); err == nil {
			l.MinYears = &n
		}
		if _, known := minYearsOptions[a.Choice]; known {
			l.Confidence["min_years"] = conf(a)
		}
	}
	return l, l.Seniority != nil && len(l.Areas) > 0
}

// minExtraction drops extraction answers the model itself doubts.
const minExtraction = 0.5

// extractionLabels reads an extraction's answers. Codes are checked and
// names a model wrote instead of a code are mapped; anything else is dropped.
func extractionLabels(r extraction.Request, raw string) Labels {
	l := Labels{Confidence: map[string]float64{}}
	answers := extraction.Answers(r, raw)
	sure := func(name string) (extraction.Answer, bool) {
		a, ok := answers[name]
		if !ok || (a.Confidence != nil && *a.Confidence < minExtraction) {
			return a, false
		}
		if a.Confidence != nil {
			l.Confidence[name] = round(*a.Confidence)
		}
		return a, true
	}
	if a, ok := sure("specialty"); ok {
		var s string
		if json.Unmarshal(a.Value, &s) == nil {
			if s = clip(strings.Join(strings.Fields(s), " "), 200); s != "" {
				l.Specialty = &s
			}
		}
	}
	list := func(name string, norm func(string) string) []string {
		a, ok := sure(name)
		if !ok {
			return nil
		}
		var vs []string
		if json.Unmarshal(a.Value, &vs) != nil {
			return nil
		}
		seen := map[string]bool{}
		out := []string{}
		for _, v := range vs {
			if c := norm(v); c != "" && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
		sort.Strings(out)
		return out
	}
	l.Countries = list("countries", CountryCode)
	l.Languages = list("languages", LanguageCode)
	if len(l.Countries) > 50 {
		l.Countries = l.Countries[:50]
	}
	if len(l.Languages) > 20 {
		l.Languages = l.Languages[:20]
	}
	return l
}

// runnerUp is the second most probable choice.
func runnerUp(a systemone.Answer) (string, float64) {
	best, p := "", -1.0
	for k, v := range a.Probabilities {
		if k == a.Choice {
			continue
		}
		if v > p || (v == p && k < best) {
			best, p = k, v
		}
	}
	return best, p
}

func contains(vs []string, v string) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }

// languages maps ISO 639-1 codes to themselves, and common names (English and
// Spanish) to codes.
var languages = func() map[string]string {
	m := map[string]string{
		"english": "en", "ingles": "en", "spanish": "es", "espanol": "es", "castellano": "es", "portuguese": "pt", "portugues": "pt",
		"french": "fr", "frances": "fr", "german": "de", "aleman": "de", "italian": "it", "italiano": "it", "dutch": "nl", "neerlandes": "nl",
		"chinese": "zh", "mandarin": "zh", "chino": "zh", "japanese": "ja", "japones": "ja", "korean": "ko", "coreano": "ko",
		"russian": "ru", "ruso": "ru", "arabic": "ar", "arabe": "ar", "hindi": "hi", "polish": "pl", "polaco": "pl", "turkish": "tr", "turco": "tr",
		"swedish": "sv", "sueco": "sv", "norwegian": "no", "danish": "da", "finnish": "fi", "greek": "el", "hebrew": "he", "ukrainian": "uk",
		"czech": "cs", "romanian": "ro", "hungarian": "hu", "vietnamese": "vi", "thai": "th", "indonesian": "id", "malay": "ms", "tagalog": "tl", "filipino": "tl",
		"catalan": "ca", "catalan language": "ca", "basque": "eu", "galician": "gl",
	}
	for _, c := range strings.Fields("en es pt fr de it nl zh ja ko ru ar hi pl tr sv no da fi el he uk cs ro hu vi th id ms tl ca eu gl bg hr sr sk sl et lv lt fa ur bn ta te") {
		m[c] = c
	}
	return m
}()

// LanguageCode reads one language as an ISO 639-1 code; "" when unknown.
func LanguageCode(s string) string {
	f := Fold(s)
	if c, ok := languages[f]; ok {
		return c
	}
	// "en-US", "pt_BR"
	if len(s) >= 2 {
		if c, ok := languages[strings.ToLower(s[:2])]; ok && (len(s) == 2 || s[2] == '-' || s[2] == '_') {
			return c
		}
	}
	return ""
}
