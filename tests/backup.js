const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const html = fs.readFileSync('cgate-server/web/console.html', 'utf8');
const start = html.indexOf("  backupBtn.addEventListener('click'");
const end = html.indexOf('\n  });', start) + '\n  });'.length;
assert(start > 0 && end > start);

async function check(saved, failed) {
  const requests = [], warnings = [], downloads = [], statuses = [];
  let click, finish;
  const complete = new Promise(resolve => { finish = resolve; });
  const context = {
    backupBtn: {addEventListener: (_, handler) => {click = handler;}},
    backupProject: 'HOME', base: '', encodeURIComponent,
    fetch: (url, options) => {
      requests.push({url, options});
      return Promise.resolve({ok: !failed,
        headers: {get: key => key === 'X-CGate-Saved' ? String(saved) : 'Save failed; disk copy may be stale'},
        blob: () => Promise.resolve({}), json: () => Promise.resolve({error: 'Snapshot failed'})});
    },
    addEntry: (...args) => warnings.push(args),
    setStatus: (...args) => statuses.push(args), stamp: () => '00:00:00',
    setBackupTarget: () => finish(), setTimeout: callback => callback(),
    URL: {createObjectURL: () => 'blob:test', revokeObjectURL: () => {}},
    document: {body: {appendChild: () => {}, removeChild: () => {}}, createElement: () => ({click: () => downloads.push(true)})}
  };
  vm.runInNewContext(html.slice(start,end), context);
  click(); await complete;
  assert.equal(requests.length, 1);
  assert.equal(requests[0].options.method, 'POST');
  assert(requests[0].url.startsWith('/tag/backup?'));
  assert.equal(downloads.length, failed ? 0 : 1);
  if (!saved && !failed) assert.equal(warnings.length, 1);
  if (failed) assert(statuses.some(s => s[0].includes('Snapshot failed')));
}
(async () => {
  await check(true, false); await check(false, false); await check(false, true);
  console.log('PASS: saved backup, explicit stale fallback, and failed snapshot without download');
})().catch(error => {console.error(error);process.exitCode=1;});
