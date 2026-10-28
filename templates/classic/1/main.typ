// classic, version 1: a single-column CV built for ATS parsing.
//
// The CV arrives as JSON in /content.json (see templates/README.md). User text
// is only ever placed as text: it is never evaluated, so markup in a highlight
// prints literally.

#let cv = json("/content.json")
#let s = json("/strings.json").at(cv.locale)

#let has(x) = x != none and x != ""
#let get(d, k) = if d == none or k == none { none } else { d.at(k, default: none) }

// Dates carry their own precision (part 2). A CV shows at most the month.
#let fmt-date(d) = {
  if d == none { return none }
  let parts = d.date.split("-")
  if d.precision == "year" or parts.len() < 2 { return parts.at(0) }
  s.months.at(int(parts.at(1)) - 1) + " " + parts.at(0)
}

#let dates(e) = {
  let start = fmt-date(get(e, "start"))
  if e.form != "engagement" {
    let end = fmt-date(get(e, "end"))
    if e.form == "credential" and end != none {
      return (start, s.expires + " " + end).filter(has).join(" · ")
    }
    return start
  }
  let end = if get(e, "current") == true { s.present } else { fmt-date(get(e, "end")) }
  if start == none { return end }
  if end == none or end == start { return start }
  start + " – " + end
}

// Integers get a thousands separator; anything else prints as given.
#let amount(n) = {
  if type(n) != int { return str(n) }
  let digits = str(calc.abs(n)).clusters().rev()
  let out = ""
  for (i, c) in digits.enumerate() {
    if i > 0 and calc.rem(i, 3) == 0 { out = s.thousands + out }
    out = c + out
  }
  if n < 0 { "-" + out } else { out }
}

#let enum-label(group, value) = get(get(s.enums, group), value)

// The muted line under an entry's title. Layout is keyed by kind (part 2).
#let subtitle(e) = {
  let d = get(e, "details")
  let g(k) = get(d, k)
  let parts = (get(e, "organization"),)
  let k = e.kind
  if k == "degree_program" {
    parts.push(g("field_of_study"))
    parts.push(enum-label("degree_status", g("status")))
    if has(g("grade")) { parts.push(s.grade + ": " + g("grade")) }
    parts.push(g("honors"))
  } else if k == "job" {
    parts.push(enum-label("employment_type", g("employment_type")))
  } else if k == "business" {
    parts.push(enum-label("business_role", g("role")))
  } else if k == "apprenticeship" {
    parts.push(g("trade"))
  } else if k == "military_service" {
    parts.push(g("branch"))
    parts.push(g("rank"))
  } else if k == "membership" {
    parts.push(enum-label("membership_role", g("role")))
  } else if k == "assignment" {
    parts.push(g("client"))
  } else if k in ("license", "certification", "clearance") {
    parts.push(g("issuer"))
    parts.push(g("level"))
    parts.push(g("jurisdiction"))
    if has(g("number")) { parts.push(s.number + " " + g("number")) }
    if has(g("credential_id")) { parts.push(s.credential_id + " " + g("credential_id")) }
    parts.push(g("score"))
  } else if k == "project" {
    parts.push(g("role"))
  } else if k == "publication" {
    let authors = g("authors")
    if authors != none and authors.len() > 0 { parts.push(authors.join(", ")) }
    parts.push(g("venue"))
    if has(g("doi")) { parts.push("doi:" + g("doi")) }
  } else if k == "presentation" {
    parts.push(g("event"))
    parts.push(enum-label("presentation_format", g("format")))
  } else if k == "patent" {
    parts.push(g("office"))
    parts.push(g("number"))
    parts.push(enum-label("patent_status", g("status")))
  } else if k == "creative_work" {
    parts.push(enum-label("creative_medium", g("medium")))
    parts.push(g("venue"))
  } else if k == "award" {
    parts.push(g("issuer"))
  } else if k == "funding" {
    parts.push(g("funder"))
    parts.push(enum-label("funding_type", g("funding_type")))
    if g("amount") != none {
      parts.push((amount(g("amount")), g("currency")).filter(has).join(" "))
    }
  }
  parts.push(get(e, "location"))
  parts.filter(has)
}

// Links are visible text, so a parser that drops annotations keeps the URL.
#let short-url(url) = {
  let u = url.trim("/", at: end)
  for prefix in ("https://", "http://", "mailto:") {
    if u.starts-with(prefix) { u = u.slice(prefix.len()) }
  }
  if u.starts-with("www.") { u = u.slice(4) }
  u
}
#let url-link(url) = link(url, short-url(url))

#let entry(e) = block(breakable: true, above: 0.95em, below: 0pt, {
  block(below: 0pt, text(weight: "semibold", e.title))
  // Dates lead the line under the title instead of sitting right-aligned: a
  // wide gap makes text extraction read them as a separate column.
  let sub = (dates(e),) + subtitle(e)
  sub = sub.filter(has)
  if sub.len() > 0 {
    block(above: 0.35em, below: 0pt, text(fill: luma(70), sub.join(" · ")))
  }
  if has(get(e, "url")) {
    block(above: 0.35em, below: 0pt, text(fill: luma(70), url-link(e.url)))
  }
  if has(get(e, "summary")) {
    block(above: 0.5em, below: 0pt, e.summary)
  }
  let highlights = get(e, "highlights")
  if highlights != none and highlights.len() > 0 {
    block(above: 0.5em, below: 0pt, list(..highlights.map(h => h.text)))
  }
  let children = get(e, "children")
  if children != none and children.len() > 0 {
    block(above: 0.8em, below: 0pt, pad(left: 1em, children.map(entry).join()))
  }
})

#let section(title, body) = {
  heading(level: 2, title)
  body
}

#let skill-line(skill) = {
  let p = get(skill, "proficiency")
  if skill.type == "language" and has(p) {
    skill.name + " (" + (if p == "native" { s.native } else { p }) + ")"
  } else {
    skill.name
  }
}

// Page and type.

#let date = {
  let p = cv.date.split("-").map(int)
  datetime(year: p.at(0), month: p.at(1), day: p.at(2))
}
#let profile = cv.profile

#set document(title: profile.name, author: profile.name, date: date)
#set page(paper: cv.at("paper", default: s.paper), margin: (x: 1.7cm, y: 1.5cm))
#set text(font: "Source Sans 3", size: 10.5pt, lang: cv.locale, hyphenate: false)
#set par(leading: 0.6em, spacing: 0.6em)
#set list(indent: 0.4em, body-indent: 0.5em, spacing: 0.45em, marker: [•])

#show heading.where(level: 1): it => block(below: 0.4em, text(size: 22pt, weight: "semibold", it.body))
#show heading.where(level: 2): it => block(above: 1.4em, below: 0.75em, {
  text(size: 11pt, weight: "semibold", tracking: 0.06em, upper(it.body))
  v(-0.55em)
  line(length: 100%, stroke: 0.6pt + luma(120))
})

// Header.

#align(center, {
  heading(level: 1, profile.name)
  if has(get(profile, "headline")) {
    block(below: 0.5em, text(size: 12pt, profile.headline))
  }
  let contact = ()
  if has(get(profile, "location")) { contact.push(profile.location) }
  if has(get(profile, "email")) { contact.push(link("mailto:" + profile.email, profile.email)) }
  if has(get(profile, "phone")) { contact.push(profile.phone) }
  let links = get(profile, "links")
  if links != none {
    for l in links {
      if has(get(l, "url")) { contact.push(url-link(l.url)) }
    }
  }
  if contact.len() > 0 { contact.join(" · ") }
})

#if has(get(profile, "summary")) {
  section(s.sections.summary, profile.summary)
}

// Sections in the order the content gives them.

#for sec in cv.sections {
  if sec.entries.len() > 0 {
    section(s.sections.at(sec.key), sec.entries.map(entry).join())
  }
}

#let skills = get(cv, "skills")
#if skills != none {
  let groups = ("hard", "tool", "soft").map(t => (t, skills.filter(x => x.type == t)))
  let lines = groups.filter(((t, xs)) => xs.len() > 0).map(((t, xs)) => {
    text(weight: "semibold", s.skill_types.at(t) + ":") + " " + xs.map(skill-line).join(", ")
  })
  if lines.len() > 0 {
    section(s.sections.skills, lines.join(linebreak()))
  }
  let languages = skills.filter(x => x.type == "language")
  if languages.len() > 0 {
    section(s.sections.languages, languages.map(skill-line).join(", "))
  }
}

// Fitting reads the page count with a query for <cv-pages>. Metadata takes no
// space and prints nothing.
#context [#metadata(counter(page).final().first()) <cv-pages>]
