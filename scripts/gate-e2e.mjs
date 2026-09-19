const base = process.env.BASE ?? 'http://127.0.0.1:19531';
const password = 'LngBalance!2026';
let failures = 0;

function check(label, cond, payload) {
  if (cond) {
    console.log('PASS', label);
  } else {
    failures += 1;
    console.error('FAIL', label, JSON.stringify(payload, null, 2));
  }
}

async function call(method, path, token, body, expectedStatus = 200) {
  const headers = { 'X-Request-ID': `gate-e2e-${Math.random().toString(36).slice(2, 8)}` };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const response = await fetch(base + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  const payload = await response.json().catch(() => null);
  if (response.status !== expectedStatus) {
    throw new Error(`${method} ${path}: expected ${expectedStatus} got ${response.status}: ${JSON.stringify(payload)}`);
  }
  return payload;
}

async function login(email) {
  const payload = await call('POST', '/api/v1/auth/login', null, { email, password });
  return payload.data.token;
}

async function main() {
  const analyst = await login('analyst@lng.local');
  const reviewer = await login('reviewer@lng.local');
  const unique = String(Date.now()).slice(-9);

  const tank = (await call('POST', '/api/v1/tanks', analyst, {
    tank_code: `GT-${unique}`, name: `gate tank ${unique}`, nominal_capacity_m3: 200000,
    min_level_m: 0, max_level_m: 20, reference_density_kgm3: 450, reference_temperature_c: -160,
    thermal_expansion_per_c: 0.0012, capacity_curve: [0, 10000], coefficient_version: 'gt-v1', tank_status: 'active'
  }, 201)).data;

  const snap = async (at, level, note, quality = 'good') =>
    (await call('POST', '/api/v1/measurements', analyst, {
      tank_id: tank.id, measured_at: at, liquid_level_m: level, liquid_temp_c: -160, vapor_pressure_kpa: 115,
      density_kgm3: 450, measurement_uncertainty_pct: 0.35, quality_flag: quality, source_note: note
    }, 201)).data;

  const transfer = async (type, start, end, mass, ref, status = 'confirmed') =>
    (await call('POST', '/api/v1/transfers', analyst, {
      tank_id: tank.id, operation_type: type, start_at: start, end_at: end, measured_mass_kg: mass,
      measurement_uncertainty_pct: 0.25, counterparty_ref: ref, operation_status: status
    }, 201)).data;

  await snap('2026-06-01T00:30:00Z', 10, 'opening');
  await snap('2026-06-02T00:30:00Z', 9.96, 'closing');
  await transfer('inflow', '2026-06-01T03:00:00Z', '2026-06-01T04:00:00Z', 100000, 'GT-IN-1');
  await transfer('outflow', '2026-06-01T05:00:00Z', '2026-06-01T06:00:00Z', 65000, 'GT-OUT-1');

  const period = { tank_id: tank.id, period_start: '2026-06-01T01:00:00Z', period_end: '2026-06-02T01:00:00Z' };
  const original = (await call('POST', '/api/v1/balances/run', analyst, period, 201)).data;
  check('run created in calculating', original.balance_status === 'calculating', original);

  // 场景一：晚录流入确认（以确认态直接登记的晚录转移）。
  await transfer('inflow', '2026-06-01T08:00:00Z', '2026-06-01T09:00:00Z', 33000, 'GT-LATE-IN');
  let flagged = (await call('GET', `/api/v1/balances/${original.id}`, analyst)).data;
  check('late confirmed inflow flags run', flagged.balance_status === 'recalculation_required', flagged);
  check('reason is late_transfer_confirmed',
    flagged.recalculation_reasons?.[0]?.code === 'late_transfer_confirmed', flagged);

  // 复核员不能接受待重算运行。
  let blocked = await call('POST', `/api/v1/balances/${original.id}/review`, reviewer, {
    target_status: 'accepted', version: flagged.version, review_note: 'must not accept'
  }, 409);
  check('review blocked with RECALCULATION_REQUIRED', blocked.error.code === 'RECALCULATION_REQUIRED', blocked);

  // 分析员也不能重新提交。
  blocked = await call('POST', `/api/v1/balances/${original.id}/submit`, analyst, { version: flagged.version }, 409);
  check('submit blocked for flagged run', blocked.error.code === 'RECALCULATION_REQUIRED', blocked);

  // 场景二：再补录更接近期末的快照，原因链应追加。
  await snap('2026-06-02T00:45:00Z', 9.94, 'later closing');
  flagged = (await call('GET', `/api/v1/balances/${original.id}`, analyst)).data;
  check('second gate appends reason', flagged.recalculation_reasons?.length === 2, flagged);
  check('second reason code', flagged.recalculation_reasons?.[1]?.code === 'boundary_snapshot_superseded', flagged);

  // 重新计算生成新记录。
  let successor = (await call('POST', `/api/v1/balances/${original.id}/recalculate`, analyst, { version: flagged.version }, 201)).data;
  check('successor created as calculating with chain',
    successor.balance_status === 'calculating' && successor.supersedes_id === original.id, successor);
  check('successor includes late inflow mass',
    Math.abs(successor.net_transfer_kg - (100000 + 33000 - 65000)) < 1e-6, successor);

  // 重复重算被拒（旧记录已挂后继且后继活跃）。
  const claimed = (await call('GET', `/api/v1/balances/${original.id}`, analyst)).data;
  blocked = await call('POST', `/api/v1/balances/${original.id}/recalculate`, analyst, { version: claimed.version }, 409);
  check('duplicate recalculation rejected', blocked.error.code === 'RECALCULATION_ALREADY_EXISTS', blocked);

  // 并发两次重算请求（旧版本号相同）只能成功一次。
  // 先把后继驳回，使旧记录可以重新申领；然后并发发起两次重算。
  successor = (await call('POST', `/api/v1/balances/${successor.id}/submit`, analyst, { version: successor.version })).data;
  const rejected = await call('POST', `/api/v1/balances/${successor.id}/review`, reviewer, {
    target_status: 'rejected', version: successor.version, review_note: 'reject first attempt to retry'
  }, 200);
  check('successor rejected independently', rejected.data.balance_status === 'rejected', rejected.data);

  const parallel = await Promise.allSettled([
    call('POST', `/api/v1/balances/${original.id}/recalculate`, analyst, { version: claimed.version }, 201),
    call('POST', `/api/v1/balances/${original.id}/recalculate`, analyst, { version: claimed.version }, 201)
  ]);
  const succeeded = parallel.filter((p) => p.status === 'fulfilled').length;
  const rejectedParallel = parallel.filter((p) => p.status === 'rejected').length;
  check('concurrent recalculation succeeds exactly once', succeeded === 1 && rejectedParallel === 1,
    parallel.map((p) => p.status));

  const winner = parallel.find((p) => p.status === 'fulfilled').value.data;
  const winnerSubmit = (await call('POST', `/api/v1/balances/${winner.id}/submit`, analyst, { version: winner.version })).data;

  // 直接对后继调 review/accepted 必须被拒绝，要求走 replace。
  blocked = await call('POST', `/api/v1/balances/${winner.id}/review`, reviewer, {
    target_status: 'accepted', version: winnerSubmit.version, review_note: 'direct accept not allowed'
  }, 409);
  check('direct successor accept rejected', blocked.error.code === 'REPLACEMENT_REQUIRED', blocked);

  // 原子替代成功。
  const replaced = await call('POST', `/api/v1/balances/${original.id}/replace`, reviewer, {
    successor_id: winner.id, predecessor_version: claimed.version + 1, successor_version: winnerSubmit.version,
    review_note: '证据重算复核通过，原子替代旧运行。'
  }, 200);
  check('successor accepted via replace', replaced.data.balance_status === 'accepted', replaced.data);
  const oldAfter = (await call('GET', `/api/v1/balances/${original.id}`, reviewer)).data;
  check('predecessor replaced', oldAfter.balance_status === 'replaced' && oldAfter.superseded_by_id === winner.id, oldAfter);

  // 重复替代只能成功一次：第二次应失败且状态不漂移。
  blocked = await call('POST', `/api/v1/balances/${original.id}/replace`, reviewer, {
    successor_id: winner.id, predecessor_version: oldAfter.version, successor_version: replaced.data.version,
    review_note: 'duplicate replacement must fail'
  }, 409);
  check('duplicate replacement rejected', blocked.error.code === 'REPLACEMENT_NOT_PENDING', blocked);

  // 场景三：罐容系数版本更新命中新运行；已替代/已接受终态不受影响。
  const run2 = (await call('POST', '/api/v1/balances/run', analyst, period, 201)).data;
  const run2Submit = (await call('POST', `/api/v1/balances/${run2.id}/submit`, analyst, { version: run2.version })).data;
  await call('PUT', `/api/v1/tanks/${tank.id}`, analyst, {
    name: tank.name, nominal_capacity_m3: tank.nominal_capacity_m3, min_level_m: 0, max_level_m: 20,
    reference_density_kgm3: 450, reference_temperature_c: -160, thermal_expansion_per_c: 0.0012,
    capacity_curve: [0, 10000], coefficient_version: 'gt-v2', tank_status: 'active', version: tank.version
  }, 200);
  const run2Flagged = (await call('GET', `/api/v1/balances/${run2.id}`, analyst)).data;
  check('coefficient update gates pending run', run2Flagged.balance_status === 'recalculation_required', run2Flagged);
  check('coefficient reason recorded', run2Flagged.recalculation_reasons?.[0]?.code === 'coefficient_version_updated', run2Flagged);

  const winnerFinal = (await call('GET', `/api/v1/balances/${winner.id}`, reviewer)).data;
  const oldFinal = (await call('GET', `/api/v1/balances/${original.id}`, reviewer)).data;
  check('accepted successor stays terminal', winnerFinal.balance_status === 'accepted', winnerFinal);
  check('replaced predecessor stays terminal', oldFinal.balance_status === 'replaced', oldFinal);

  // invalid 快照不命中闸门。
  const run3 = (await call('POST', '/api/v1/balances/run', analyst, period, 201)).data;
  await snap('2026-06-02T00:50:00Z', 9.93, 'invalid late snapshot', 'invalid');
  const run3After = (await call('GET', `/api/v1/balances/${run3.id}`, analyst)).data;
  check('invalid snapshot does not gate', run3After.balance_status === 'calculating', run3After);

  // 审计包含闸门与替代事件。
  const audits = (await call('GET', '/api/v1/audits?page=1&page_size=100&entity_type=balance_run', reviewer)).data;
  const actions = audits.map((event) => event.action);
  check('audit has recalculation_required', actions.includes('balance_run.recalculation_required'), actions);
  check('audit has recalculation_claimed', actions.includes('balance_run.recalculation_claimed'), actions);
  check('audit has replacement_accepted', actions.includes('balance_run.replacement_accepted'), actions);
  check('audit has replaced', actions.includes('balance_run.replaced'), actions);

  if (failures > 0) {
    console.error(`GATE_E2E_FAIL with ${failures} failures`);
    process.exit(1);
  }
  console.log('GATE_E2E_PASS');
}

main().catch((error) => {
  console.error('GATE_E2E_ERROR', error.stack ?? error);
  process.exit(1);
});
