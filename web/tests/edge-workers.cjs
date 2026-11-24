// The edge worker form turns a text box into the host list the API validates.
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const assert = require('node:assert/strict');
const ts = require('typescript');

const source = fs.readFileSync(path.join(__dirname, '../src/app/admin/edge-workers.ts'), 'utf8');
const fn = /export function parseHosts[\s\S]*?\n}\n/.exec(source);
assert.ok(fn, 'parseHosts is exported');
const parseHosts = new Function(`${ts.transpileModule(fn[0].replace('export ', ''), {}).outputText}; return parseHosts;`)();

test('parseHosts splits on lines, commas and spaces, lowercases and de-duplicates', () => {
  assert.deepEqual(parseHosts('linkedin.com\nIndeed.com, linkedin.com  glassdoor.com.'), ['linkedin.com', 'indeed.com', 'glassdoor.com']);
  assert.deepEqual(parseHosts('  \n '), []);
});

test('the Scraping page offers the edge provider and lists the workers', () => {
  const settings = fs.readFileSync(path.join(__dirname, '../src/app/admin/scraper-settings.ts'), 'utf8');
  assert.match(settings, /zValue="edge"/);
  const page = fs.readFileSync(path.join(__dirname, '../src/app/admin/admin.page.html'), 'utf8');
  assert.match(page, /<app-edge-workers \/>/);
});
