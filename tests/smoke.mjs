// Smoke test: boots the forge, exercises REST + git (real git CLI) +
// GraphQL + MCP, then kills it. Exit 0 when every assertion passes.
// Usage: node tests/smoke.mjs
import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = join(HERE, '..');
const FORGE = join(ROOT, 'forge');
const TOKEN = 'smoke-test-token';

let failures = 0;
function check(name, cond, extra = '') {
  if (cond) console.log(`ok   ${name}`);
  else {
    failures += 1;
    console.log(`FAIL ${name} ${extra}`);
  }
}

const base = { headers: { authorization: `Bearer ${TOKEN}`, 'content-type': 'application/json' } };
async function req(method, path, body) {
  const res = await fetch(`http://127.0.0.1:${PORT}${path}`, {
    method,
    headers: base.headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let json = null;
  try { json = JSON.parse(text); } catch { /* non-JSON */ }
  return { status: res.status, text, json, headers: res.headers };
}

function git(args, cwd, env = {}) {
  return execFileSync('git', args, {
    cwd,
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: 'Conformance',
      GIT_AUTHOR_EMAIL: 'conformance@make.fun',
      GIT_COMMITTER_NAME: 'Conformance',
      GIT_COMMITTER_EMAIL: 'conformance@make.fun',
      GIT_AUTHOR_DATE: '2025-01-01T00:00:00Z',
      GIT_COMMITTER_DATE: '2025-01-01T00:00:00Z',
      ...env,
    },
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
}

// --- boot ----------------------------------------------------------------------
const child = spawn(FORGE, [], { stdio: ['ignore', 'pipe', 'inherit'] });
let PORT = 0;
await new Promise((resolve, reject) => {
  let buf = '';
  const timer = setTimeout(() => reject(new Error('no PORT line')), 10000);
  child.stdout.on('data', (c) => {
    buf += c.toString();
    const m = buf.match(/PORT (\d+)/);
    if (m) { clearTimeout(timer); PORT = Number(m[1]); resolve(); }
  });
  child.once('exit', (code) => { clearTimeout(timer); reject(new Error(`forge exited ${code}`)); });
});
console.log(`forge on ${PORT}`);

try {
  // --- REST --------------------------------------------------------------------
  let r = await req('GET', '/healthz');
  check('healthz 200', r.status === 200 && r.json?.ok === true, r.text);

  r = await req('POST', '/api/v3/user/repos', { name: 'smoke', private: false });
  check('create repo 201', r.status === 201 && r.json?.name === 'smoke', r.text);

  r = await req('POST', '/api/v3/user/repos', { name: 'smoke' });
  check('duplicate repo 422', r.status === 422, r.text);

  r = await req('GET', '/api/v3/user');
  check('viewer login', r.status === 200 && r.json?.login === 'adaiversen', r.text);

  r = await fetch(`http://127.0.0.1:${PORT}/api/v3/repos/adaiversen/smoke`);
  check('anon repo 401', r.status === 401);

  r = await req('GET', '/api/v3/repos/olgabauer/smoke');
  check('other owner 404', r.status === 404);

  r = await req('POST', '/api/v3/repos/adaiversen/smoke/labels', { name: 'bug', color: 'd73a4a' });
  check('create label 201', r.status === 201 && r.json?.name === 'bug', r.text);

  r = await req('POST', '/api/v3/repos/adaiversen/smoke/issues', { title: 'First issue', body: 'hello', labels: ['bug'] });
  check('create issue 201', r.status === 201 && r.json?.number === 1, r.text);
  const issueId = r.json?.node_id;

  r = await req('POST', '/api/v3/repos/adaiversen/smoke/issues', { title: 'Bad label', labels: ['nope'] });
  check('unknown label 422', r.status === 422);

  r = await req('GET', '/api/v3/repos/adaiversen/smoke/issues/1');
  check('get issue', r.status === 200 && r.json?.title === 'First issue', r.text);

  r = await req('POST', '/api/v3/repos/adaiversen/smoke/issues/1/comments', { body: 'a comment' });
  check('comment 201', r.status === 201 && r.json?.body === 'a comment', r.text);

  r = await req('PATCH', '/api/v3/repos/adaiversen/smoke/issues/1', { state: 'closed' });
  check('close issue', r.status === 200 && r.json?.state === 'closed', r.text);

  r = await req('GET', '/api/v3/repos/adaiversen/smoke/issues?state=open');
  check('open issues empty after close', r.status === 200 && Array.isArray(r.json) && r.json.length === 0, r.text);

  // contents
  const b64 = Buffer.from('token data\n').toString('base64');
  r = await req('PUT', '/api/v3/repos/adaiversen/smoke/contents/config/token.css', { message: 'add token', content: b64 });
  check('contents PUT 201', r.status === 201 && r.json?.content?.sha, r.text);
  const contentSha = r.json?.content?.sha;

  r = await req('GET', '/api/v3/repos/adaiversen/smoke/contents/config/token.css');
  check('contents GET file', r.status === 200 && Buffer.from(r.json?.content ?? '', 'base64').toString() === 'token data\n', r.text);

  r = await req('GET', '/api/v3/repos/adaiversen/smoke/contents/config');
  check('contents GET dir', r.status === 200 && Array.isArray(r.json) && r.json.length === 1 && r.json[0].name === 'token.css', r.text);

  r = await req('PUT', '/api/v3/repos/adaiversen/smoke/contents/config/token.css', { message: 'stale', content: b64, sha: '0000000000000000000000000000000000000000' });
  check('contents stale sha 409', r.status === 409, r.text);

  r = await req('PUT', '/api/v3/repos/adaiversen/smoke/contents/config/token.css', { message: 'update', content: Buffer.from('token data v2\n').toString('base64'), sha: contentSha });
  check('contents PUT 200 update', r.status === 200, r.text);

  r = await req('GET', '/api/v3/repos/adaiversen/smoke/branches');
  check('branches list has main', r.status === 200 && r.json?.some((b) => b.name === 'main'), r.text);

  r = await req('GET', '/api/v3/repos/adaiversen/smoke/commits');
  check('commits list', r.status === 200 && r.json?.length === 2, r.text);

  // --- git (real client) ---------------------------------------------------------
  const work = mkdtempSync(join(tmpdir(), 'forge-smoke-'));
  const w1 = join(work, 'w1');
  git(['clone', `http://127.0.0.1:${PORT}/adaiversen/smoke.git`, w1], work);
  check('git clone', true);
  const headRef = git(['symbolic-ref', 'HEAD'], w1).trim();
  check('clone HEAD is main', headRef === 'refs/heads/main', headRef);
  const cloned = (await import('node:fs')).readFileSync(join(w1, 'config/token.css'), 'utf8');
  check('clone has contents file', cloned === 'token data v2\n', JSON.stringify(cloned));

  git(['checkout', '-qb', 'feature'], w1);
  (await import('node:fs')).writeFileSync(join(w1, 'feature.txt'), 'feature work\n');
  git(['add', '.'], w1);
  git(['commit', '-qm', 'feature work'], w1);
  git(['push', 'origin', 'feature'], w1);
  check('git push feature', true);

  r = await req('POST', '/api/v3/repos/adaiversen/smoke/pulls', { title: 'Feature PR', head: 'feature', base: 'main' });
  check('create PR 201', r.status === 201 && r.json?.number === 2, r.text);
  const prNumber = r.json?.number;

  r = await req('GET', `/api/v3/repos/adaiversen/smoke/pulls/${prNumber}/files`);
  check('PR files', r.status === 200 && r.json?.length === 1 && r.json[0].filename === 'feature.txt' && r.json[0].status === 'added', r.text);

  r = await req('PUT', `/api/v3/repos/adaiversen/smoke/pulls/${prNumber}/merge`, { merge_method: 'squash' });
  check('merge PR', r.status === 200 && r.json?.merged === true, r.text);

  r = await req('GET', '/api/v3/repos/adaiversen/smoke/contents/feature.txt?ref=main');
  check('merged file on main', r.status === 200 && Buffer.from(r.json?.content ?? '', 'base64').toString() === 'feature work\n', r.text);

  // shallow clone
  const w2 = join(work, 'w2');
  git(['clone', '--depth', '1', `http://127.0.0.1:${PORT}/adaiversen/smoke.git`, w2], work);
  check('shallow clone', git(['rev-list', '--count', 'HEAD'], w2).trim() === '1');
  rmSync(work, { recursive: true, force: true });

  // --- GraphQL -------------------------------------------------------------------
  async function gql(query, variables) {
    const res = await fetch(`http://127.0.0.1:${PORT}/api/graphql`, {
      method: 'POST', headers: base.headers, body: JSON.stringify({ query, variables }),
    });
    return { status: res.status, json: await res.json() };
  }
  let g = await gql('query ViewerLogin { viewer { login } }');
  check('gql viewer', g.status === 200 && g.json?.data?.viewer?.login === 'adaiversen', JSON.stringify(g.json));

  g = await gql('query RepositoryFields($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id databaseId name owner { login } defaultBranchRef { name } viewerPermission hasIssuesEnabled } }', { owner: 'adaiversen', name: 'smoke' });
  check('gql repository fields', g.json?.data?.repository?.name === 'smoke' && g.json?.data?.repository?.viewerPermission === 'ADMIN' && g.json?.data?.repository?.defaultBranchRef?.name === 'main', JSON.stringify(g.json));

  g = await gql('query { Issue: __type(name: "Issue") { fields(includeDeprecated: true) { name } } }');
  const issueFields = (g.json?.data?.Issue?.fields ?? []).map((f) => f.name);
  check('gql Issue fields', issueFields.includes('number') && issueFields.includes('title') && issueFields.includes('state'), JSON.stringify(issueFields));

  g = await gql('mutation IssueCreate($input: CreateIssueInput!) { createIssue(input: $input) { issue { number id } } }', { input: { repositoryId: (await gql('query { repository(owner: "adaiversen", name: "smoke") { id } }')).json.data.repository.id, title: 'GQL issue', body: 'via graphql' } });
  check('gql createIssue', g.json?.data?.createIssue?.issue?.number === 3, JSON.stringify(g.json));

  g = await gql('query { viewer { login noSuchFieldOnViewer } }');
  check('gql invalid field errors', g.status === 200 && Array.isArray(g.json?.errors) && g.json.errors.some((e) => e.message.includes('noSuchFieldOnViewer')), JSON.stringify(g.json));

  const repoId = (await gql('query { repository(owner: "adaiversen", name: "smoke") { id } }')).json.data.repository.id;
  // Pagination needs a repo with exactly 3 issues and no PRs in between.
  await req('POST', '/api/v3/user/repos', { name: 'pages', private: false });
  for (let i = 1; i <= 3; i++) {
    await req('POST', '/api/v3/repos/adaiversen/pages/issues', { title: `Page issue ${i}` });
  }
  g = await gql('query IssuesPage($owner: String!, $name: String!, $first: Int!, $after: String) { repository(owner: $owner, name: $name) { issues(first: $first, after: $after) { nodes { number } pageInfo { hasNextPage endCursor } } } }', { owner: 'adaiversen', name: 'pages', first: 2, after: null });
  check('gql issues page 1', g.json?.data?.repository?.issues?.nodes?.length === 2 && g.json.data.repository.issues.pageInfo.hasNextPage === true, JSON.stringify(g.json));
  const cursor = g.json?.data?.repository?.issues?.pageInfo?.endCursor;
  g = await gql('query IssuesPage($owner: String!, $name: String!, $first: Int!, $after: String) { repository(owner: $owner, name: $name) { issues(first: $first, after: $after) { nodes { number } pageInfo { hasNextPage endCursor } } } }', { owner: 'adaiversen', name: 'pages', first: 2, after: cursor });
  check('gql issues page 2', g.json?.data?.repository?.issues?.nodes?.length === 1 && g.json.data.repository.issues.pageInfo.hasNextPage === false, JSON.stringify(g.json));

  // --- MCP -----------------------------------------------------------------------
  async function mcp(body, auth = true) {
    const headers = { 'content-type': 'application/json', accept: 'application/json, text/event-stream' };
    if (auth) headers.authorization = `Bearer ${TOKEN}`;
    const res = await fetch(`http://127.0.0.1:${PORT}/mcp`, { method: 'POST', headers, body: JSON.stringify(body) });
    const text = await res.text();
    let json = null;
    try { json = JSON.parse(text); } catch { /* empty */ }
    return { status: res.status, json, text };
  }
  const mcpHeaders = { 'MCP-Protocol-Version': '2026-07-28', 'Mcp-Method': 'server/discover' };
  let m = await mcp({ jsonrpc: '2.0', id: 1, method: 'server/discover', params: {}, _meta: { 'io.modelcontextprotocol/protocolVersion': '2026-07-28' }, protocolVersion: '2026-07-28' });
  // (headers above are not sent by the helper; discover must work with the meta fields alone)
  check('mcp discover', m.status === 200 && Array.isArray(m.json?.result?.supportedVersions) && m.json.result.supportedVersions.includes('2026-07-28'), m.text);

  m = await mcp({ jsonrpc: '2.0', id: 2, method: 'tools/list', params: {} });
  const tools = m.json?.result?.tools ?? [];
  check('mcp tools/list has 6 required', ['create_repo', 'get_repo', 'create_issue', 'list_issues', 'create_pull_request', 'merge_pull_request'].every((n) => tools.some((t) => t.name === n)), m.text);

  m = await mcp({ jsonrpc: '2.0', id: 3, method: 'tools/call', params: { name: 'create_repo', arguments: { name: 'mcp-repo' } } });
  check('mcp create_repo', m.status === 200 && m.json?.result?.structuredContent?.name === 'mcp-repo', m.text);

  m = await mcp({ jsonrpc: '2.0', id: 4, method: 'tools/call', params: { name: 'create_issue', arguments: { owner: 'adaiversen', repo: 'mcp-repo', title: 'MCP issue' } } });
  check('mcp create_issue', m.status === 200 && m.json?.result?.structuredContent?.title === 'MCP issue', m.text);

  m = await mcp({ jsonrpc: '2.0', id: 5, method: 'tools/call', params: { name: 'list_issues', arguments: { owner: 'adaiversen', repo: 'mcp-repo', state: 'open' } } });
  check('mcp list_issues', m.status === 200 && m.json?.result?.structuredContent?.issues?.length === 1, m.text);

  m = await mcp({ jsonrpc: '2.0', id: 6, method: 'tools/call', params: { name: 'no_such_tool', arguments: {} } });
  check('mcp unknown tool -32602', m.json?.error?.code === -32602, m.text);

  m = await mcp({ jsonrpc: '2.0', id: 7, method: 'tools/call', params: { name: 'create_issue', arguments: {} } }, false);
  check('mcp no auth 401', m.status === 401, m.text);

  const badRes = await fetch(`http://127.0.0.1:${PORT}/mcp`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${TOKEN}` },
    body: '{"jsonrpc": "2.0", "id": 424242, "method": "tools/list"',
  });
  const badJson = await badRes.json();
  check('mcp parse error 400/-32700', badRes.status === 400 && badJson?.error?.code === -32700 && badJson?.id === 424242, JSON.stringify(badJson));

  // batch GraphQL
  const batchRes = await fetch(`http://127.0.0.1:${PORT}/api/graphql`, {
    method: 'POST', headers: base.headers,
    body: JSON.stringify([{ query: 'query { viewer { login } }' }, { query: 'query { viewer { login } }' }]),
  });
  const batch = await batchRes.json();
  check('gql batch', Array.isArray(batch) && batch.length === 2 && batch[0]?.data?.viewer?.login === 'adaiversen', JSON.stringify(batch));

  // malformed JSON -> 400
  const malRes = await fetch(`http://127.0.0.1:${PORT}/api/v3/user/repos`, {
    method: 'POST', headers: base.headers, body: '{"name":',
  });
  check('malformed JSON 400', malRes.status === 400);

  // unknown route -> JSON 404
  r = await req('GET', '/api/v3/nope');
  check('unknown route 404 json', r.status === 404 && typeof r.json?.message === 'string', r.text);
} finally {
  child.kill('SIGKILL');
}

console.log(failures === 0 ? 'SMOKE OK' : `SMOKE FAILED: ${failures} failure(s)`);
process.exit(failures === 0 ? 0 : 1);
