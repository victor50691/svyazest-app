// «Связь Есть?» agent page: one state object from /ui/api/events (SSE,
// polling as a fallback), views rendered from it. No framework, no CDN;
// every text goes in through textContent.
'use strict';

(() => {
  // ---------- tiny DOM helper ----------
  function h(tag, attrs, ...kids) {
    const el = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v === null || v === undefined || v === false) continue;
        if (k === 'class') el.className = v;
        else if (k === 'text') el.textContent = v;
        else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
        else if (k === 'dataset') Object.assign(el.dataset, v);
        else el.setAttribute(k, v === true ? '' : v);
      }
    }
    for (const kid of kids.flat()) {
      if (kid === null || kid === undefined || kid === false) continue;
      el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
    }
    return el;
  }
  const $ = (s) => document.querySelector(s);

  // ---------- API ----------
  async function api(path, body) {
    const opt = { headers: { 'X-Svyazest': '1' }, credentials: 'same-origin' };
    if (body !== undefined) {
      opt.method = 'POST';
      opt.headers['Content-Type'] = 'application/json';
      opt.body = JSON.stringify(body);
    }
    let resp;
    try {
      resp = await fetch(path, opt);
    } catch (e) {
      throw new Error('Нет связи с агентом');
    }
    let data = {};
    try { data = await resp.json(); } catch (_) { /* empty */ }
    if (resp.status === 401 && data.auth) { checkAuth(); throw new Error(data.error || 'Нужен вход'); }
    if (!resp.ok) throw new Error(data.error || ('HTTP ' + resp.status));
    return data;
  }

  // ---------- formatting ----------
  const money = (v) => (v === null || v === undefined || v === '' ? '—' : Number(v).toFixed(4) + ' USD');
  function traffic(b) {
    if (b === null || b === undefined) return '—';
    b = Number(b);
    if (b < 1024 * 1024) return (b / 1024).toFixed(0) + ' КБ';
    if (b < 1024 * 1024 * 1024) return (b / 1048576).toFixed(1) + ' МБ';
    return (b / 1073741824).toFixed(2) + ' ГБ';
  }
  const two = (n) => String(n).padStart(2, '0');
  function ago(ts) {
    if (!ts) return '—';
    const s = Math.max(0, Math.round((Date.now() - new Date(ts).getTime()) / 1000));
    if (s < 10) return 'только что';
    if (s < 60) return s + ' с назад';
    if (s < 3600) return Math.floor(s / 60) + ' мин назад';
    if (s < 86400) return Math.floor(s / 3600) + ' ч назад';
    const d = new Date(ts);
    return two(d.getDate()) + '.' + two(d.getMonth() + 1) + ' ' + two(d.getHours()) + ':' + two(d.getMinutes());
  }
  function dur(ts) {
    const s = Math.max(0, Math.round((Date.now() - new Date(ts).getTime()) / 1000));
    if (s < 60) return s + ' с';
    return Math.floor(s / 60) + ' мин ' + two(s % 60) + ' с';
  }
  const routeLabel = (r) => {
    if (!r) return 'ещё не было запросов';
    if (r === 'cellular') return 'модем (запасной путь)';
    const [kind, name] = r.split(':');
    const k = { ethernet: 'Ethernet', wifi: 'Wi-Fi', default: 'по умолчанию', other: 'сеть' }[kind] || kind;
    return name ? k + ' · ' + name : k;
  };

  // ---------- toast & modal ----------
  let toastTimer = null;
  function toast(msg) {
    const t = $('#toast');
    t.textContent = msg;
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { t.hidden = true; }, 3800);
  }
  function modal(content, opts = {}) {
    const root = $('#modal-root');
    const box = h('div', { class: 'modal' + (opts.wide ? ' wide' : '') }, content);
    const bg = h('div', { class: 'modal-bg', onclick: (e) => { if (e.target === bg) close(); } }, box);
    function close() { bg.remove(); document.removeEventListener('keydown', esc); }
    function esc(e) { if (e.key === 'Escape') close(); }
    document.addEventListener('keydown', esc);
    root.append(bg);
    return close;
  }
  function confirmBox(title, text, okText, danger) {
    return new Promise((resolve) => {
      let close;
      const done = (v) => { close(); resolve(v); };
      close = modal([
        h('h3', { text: title }),
        h('p', { text }),
        h('div', { class: 'actions' },
          h('button', { class: 'btn ghost small', type: 'button', onclick: () => done(false), text: 'Отмена' }),
          h('button', { class: 'btn small' + (danger ? ' dangerous' : ''), type: 'button', onclick: () => done(true), text: okText })),
      ]);
    });
  }

  // ---------- grouped list ----------
  function section(header, rows, note) {
    return h('div', { class: 'section' },
      header ? h('div', { class: 'section-h', text: header }) : null,
      h('div', { class: 'group' }, rows.filter(Boolean)),
      note ? h('div', { class: 'note', text: note }) : null);
  }
  function row(label, value, o = {}) {
    let val = null;
    if (value instanceof Node) {
      val = value;
      if (o.cls) val.classList.add(...o.cls.split(' '));
    } else if (value !== undefined) {
      val = h('div', { class: 'value' + (o.cls ? ' ' + o.cls : '') + (o.mono ? ' mono' : ''), text: value ?? '—' });
    }
    const r = h('div', { class: 'row' + (o.onclick || o.href ? ' tap' : '') + (o.kind ? ' ' + o.kind : ''), onclick: o.onclick },
      h('div', { class: 'label' }, label, o.sub ? h('div', { class: 'sub', text: o.sub }) : null),
      val || o.chev ? h('div', { class: 'value-wrap' }, val, o.chev ? h('span', { class: 'chev', text: '›' }) : null) : null);
    if (o.href) {
      r.addEventListener('click', () => window.open(o.href, '_blank', 'noopener'));
    }
    return r;
  }

  // ---------- state ----------
  let S = null;          // last state from the agent
  let auth = null;       // page auth status
  let tab = localStorage.getItem('tab') || 'home';
  let lastKey = '';
  let history = null;
  let modems = null;
  let es = null;
  let pollTimer = null;

  function setTab(t) {
    tab = t;
    localStorage.setItem('tab', t);
    lastKey = '';
    render(true);
    if (t === 'history') loadHistory();
    if (t === 'more') loadModems();
  }
  document.querySelectorAll('.tab').forEach((b) => b.addEventListener('click', () => setTab(b.dataset.tab)));

  async function checkAuth() {
    try {
      const r = await fetch('/ui/api/auth', { credentials: 'same-origin' });
      auth = await r.json();
    } catch (_) {
      auth = null;
    }
    if (auth && auth.authed) connect();
    else stopStream();
    lastKey = '';
    render(true);
  }

  function stopStream() {
    if (es) { es.close(); es = null; }
    clearInterval(pollTimer);
    pollTimer = null;
  }

  function connect() {
    stopStream();
    if (window.EventSource) {
      es = new EventSource('/ui/api/events');
      es.addEventListener('state', (e) => { $('#conn').hidden = true; onState(JSON.parse(e.data)); });
      es.onerror = () => {
        $('#conn').hidden = false;
        // A 401 closes the stream for good: re-check the page auth.
        if (es && es.readyState === EventSource.CLOSED) setTimeout(checkAuth, 2000);
      };
    } else {
      const poll = async () => {
        try { onState(await api('/ui/api/state')); $('#conn').hidden = true; } catch (_) { $('#conn').hidden = false; }
      };
      poll();
      pollTimer = setInterval(poll, 3000);
    }
  }

  let prevJobs = null;
  function onState(st) {
    const prevLogged = S && S.status.logged_in;
    S = st;
    const done = st.status.jobs_done;
    if (prevJobs !== null && done !== prevJobs && tab === 'history') loadHistory();
    prevJobs = done;
    if (prevLogged === false && st.status.logged_in) toast('Вход выполнен');
    checkAchievements();
    render(false);
  }

  // ---------- render ----------
  function render(force) {
    const view = $('#view');
    const tabs = $('#tabs');
    let key, build;
    if (!auth) {
      key = 'noauth';
      build = () => h('div', { class: 'empty' }, 'Агент не отвечает. ', h('a', { href: '#', onclick: (e) => { e.preventDefault(); checkAuth(); }, text: 'Повторить' }));
    } else if (!auth.authed) {
      key = 'auth:' + JSON.stringify(auth);
      build = viewPageAuth;
    } else if (!S) {
      key = 'wait';
      build = () => h('div', { class: 'loading' }, h('span', { class: 'spinner' }));
    } else if (!S.status.logged_in) {
      key = 'login:' + JSON.stringify([S.login, S.status.notice]);
      build = viewLogin;
    } else {
      key = tab + ':' + JSON.stringify(tabKey());
      build = { home: viewHome, history: viewHistory, more: viewMore }[tab] || viewHome;
    }
    tabs.hidden = !(auth && auth.authed && S && S.status.logged_in);
    document.querySelectorAll('.tab').forEach((b) => b.classList.toggle('active', b.dataset.tab === tab));
    const sub = $('#brand-sub');
    sub.textContent = S ? (S.about.model || '') : '';
    if (!force && key === lastKey) return;
    lastKey = key;
    // keep the scroll position and focused field across live re-renders
    const y = window.scrollY;
    const focusId = document.activeElement && document.activeElement.id;
    view.replaceChildren(build());
    if (force) view.classList.remove('view'), void view.offsetWidth, view.classList.add('view');
    window.scrollTo(0, y);
    if (focusId && document.getElementById(focusId)) document.getElementById(focusId).focus();
  }

  function tabKey() {
    const st = { ...S.status };
    delete st.since; delete st.started;
    if (tab === 'home') return [st, S.profile];
    if (tab === 'history') return [history, S.status.jobs_done];
    return [st.attest, st.route, st.diag, st.diag_on, st.executor_id, S.about, modems, S.status.modem];
  }

  // ---------- page password ----------
  function viewPageAuth() {
    const err = h('div', { class: 'err' });
    if (!auth.password_set && auth.code_login) {
      // 1.0.7: the setup code is the password until an own one is set.
      const c = h('input', { class: 'input mono', id: 'pa-code', placeholder: 'XXXX-XXXX', autocomplete: 'off', spellcheck: 'false', autofocus: true });
      const btn = h('button', { class: 'btn block', type: 'submit', text: 'Войти' });
      return h('form', { class: 'center', onsubmit: async (e) => {
        e.preventDefault();
        err.textContent = '';
        btn.disabled = true;
        try { await api('/ui/api/auth/login', { password: c.value }); checkAuth(); } catch (ex) { err.textContent = ex.message; btn.disabled = false; c.select(); }
      } },
      h('h1', { class: 'title', text: 'Страница агента' }),
      h('p', { class: 'lead', text: 'Введите код настройки — это пароль страницы. Код показывает меню агента на этом компьютере: «sudo svyazest-agent» (в Docker — «sudo docker exec -it svyazest-agent svyazest-agent»).' }),
      h('div', { class: 'field' }, c), btn, err,
      h('p', { class: 'foot', text: 'Свой пароль вместо кода можно задать потом: «Ещё → Задать свой пароль».' }));
    }
    if (!auth.password_set) {
      const code = auth.need_setup_code ? h('input', { class: 'input mono', id: 'pa-code', placeholder: 'XXXX-XXXX', autocomplete: 'off', spellcheck: 'false' }) : null;
      const p1 = h('input', { class: 'input', id: 'pa-p1', type: 'password', placeholder: 'Не короче 8 символов', autocomplete: 'new-password' });
      const p2 = h('input', { class: 'input', id: 'pa-p2', type: 'password', placeholder: 'Ещё раз', autocomplete: 'new-password' });
      const btn = h('button', { class: 'btn block', type: 'submit', text: 'Сохранить пароль' });
      const form = h('form', { class: 'center', onsubmit: async (e) => {
        e.preventDefault();
        err.textContent = '';
        if (p1.value !== p2.value) { err.textContent = 'Пароли не совпадают'; return; }
        btn.disabled = true;
        try {
          await api('/ui/api/auth/setup', { password: p1.value, code: code ? code.value : '' });
          toast('Пароль страницы задан');
          checkAuth();
        } catch (ex) { err.textContent = ex.message; btn.disabled = false; }
      } },
      h('h1', { class: 'title', text: 'Защитите страницу' }),
      h('p', { class: 'lead', text: 'Страница агента открыта в локальной сети. Задайте пароль, чтобы управлять агентом могли только вы.' }),
      code ? h('div', { class: 'field' }, h('label', { for: 'pa-code', text: 'Код настройки' }), code,
        h('div', { class: 'note', text: 'Код показывает команда «sudo svyazest-agent status» на этом компьютере (в Docker — «sudo docker exec svyazest-agent svyazest-agent status»).' })) : null,
      h('div', { class: 'field' }, h('label', { for: 'pa-p1', text: 'Пароль' }), p1),
      h('div', { class: 'field' }, h('label', { for: 'pa-p2', text: 'Повторите пароль' }), p2),
      btn, err);
      return form;
    }
    const p = h('input', { class: 'input', id: 'pa-pass', type: 'password', placeholder: 'Пароль', autocomplete: 'current-password', autofocus: true });
    const btn = h('button', { class: 'btn block', type: 'submit', text: 'Войти' });
    return h('form', { class: 'center', onsubmit: async (e) => {
      e.preventDefault();
      err.textContent = '';
      btn.disabled = true;
      try { await api('/ui/api/auth/login', { password: p.value }); checkAuth(); } catch (ex) { err.textContent = ex.message; btn.disabled = false; p.select(); }
    } },
    h('h1', { class: 'title', text: 'Страница агента' }),
    h('p', { class: 'lead', text: 'Введите пароль, заданный для этой страницы.' }),
    h('div', { class: 'field' }, p), btn, err,
    h('p', { class: 'foot', text: 'Забыли пароль? Задайте новый на этом компьютере: «sudo svyazest-agent ui-password».' }));
  }

  // ---------- sign-in ----------
  let manualOpen = false;
  function viewLogin() {
    const L = S.login || { status: 'idle' };
    const err = h('div', { class: 'err', text: L.error || '' });
    const box = h('div', { class: 'center' });
    box.append(h('h1', { class: 'title', text: 'Вход' }));
    if (S.status.notice) box.append(h('div', { class: 'notice', text: S.status.notice }));
    if (L.status === 'waiting') {
      box.append(
        h('p', { class: 'lead', text: 'Отсканируйте QR-код камерой телефона или откройте ссылку на этом компьютере — в боте «Связь Есть?» нажмите «Start» и подтвердите вход.' }),
        h('div', { class: 'qr-wrap' },
          h('img', { class: 'qr', src: '/ui/api/login/qr.svg?u=' + encodeURIComponent(L.expires_at || ''), alt: 'QR-код для входа' }),
          h('div', { class: 'waiting' }, h('span', { class: 'spinner small' }), 'Ожидаем подтверждение в Telegram…'),
          h('div', { class: 'muted', id: 'login-left', dataset: { exp: L.expires_at || '' } })),
        h('div', { class: 'stack' },
          h('a', { class: 'btn block', href: L.url, target: '_blank', rel: 'noopener', text: 'Открыть Telegram' }),
          h('button', { class: 'btn outline block', type: 'button', text: 'Отмена', onclick: () => api('/ui/api/login/cancel', {}).catch((e) => toast(e.message)) })));
    } else {
      const btn = h('button', { class: 'btn block', type: 'button', text: L.status === 'starting' ? 'Подключение…' : 'Войти через Telegram', disabled: L.status === 'starting' });
      btn.addEventListener('click', async () => {
        btn.disabled = true;
        btn.textContent = 'Подключение…';
        try { await api('/ui/api/login/start', {}); } catch (e) { err.textContent = e.message; btn.disabled = false; btn.textContent = 'Войти через Telegram'; }
      });
      box.append(h('p', { class: 'lead', text: 'Войдите через свой аккаунт Telegram: откроется бот «Связь Есть?», в нём нажмите «Start».' }), btn, err);
    }
    // manual code
    const codeIn = h('input', { class: 'input code', id: 'login-code', maxlength: '8', placeholder: 'Код', autocomplete: 'off', spellcheck: 'false' });
    codeIn.addEventListener('input', () => { codeIn.value = codeIn.value.toUpperCase().replace(/[^A-Z0-9]/g, ''); });
    const codeErr = h('div', { class: 'err' });
    const codeBtn = h('button', { class: 'btn outline block', type: 'submit', text: 'Подключить по коду' });
    const manual = h('form', { class: 'stack', hidden: !manualOpen, onsubmit: async (e) => {
      e.preventDefault();
      codeErr.textContent = '';
      codeBtn.disabled = true;
      try { await api('/ui/api/login/code', { code: codeIn.value }); } catch (ex) { codeErr.textContent = ex.message; codeBtn.disabled = false; }
    } },
    h('div', { class: 'muted', text: 'Код выдаёт бот по кнопке «Код для входа», он действует 10 минут.' }), codeIn, codeBtn, codeErr);
    const toggle = h('button', { class: 'btn ghost block', type: 'button', text: 'Ввести код вручную', hidden: manualOpen, onclick: () => {
      manualOpen = true; manual.hidden = false; toggle.hidden = true; codeIn.focus();
    } });
    box.append(h('div', { class: 'divider', text: 'или' }), toggle, manual,
      h('p', { class: 'foot', text: 'Агент выполняет проверки только через мобильный модем. Задания можно остановить в любой момент.' }));
    return box;
  }

  // ---------- home ----------
  function signalBars(p) {
    const n = p < 0 ? 0 : p >= 75 ? 4 : p >= 50 ? 3 : p >= 25 ? 2 : p > 0 ? 1 : 0;
    return h('span', { class: 'bars' }, [1, 2, 3, 4].map((i) => h('i', { class: i <= n ? 'on' : '' })));
  }

  function viewHome() {
    const st = S.status;
    const P = S.profile || {};
    const out = h('div');
    const hero = h('div', { class: 'hero tone-' + (st.tone || 'idle') },
      h('div', { class: 'hero-main' },
        h('div', { class: 'headline' }, h('span', { class: 'dot' }), st.headline || '—'),
        h('div', { class: 'subtitle', text: st.text || '' })),
      h('div', { class: 'route-pill' }, 'Связь с сервером: ', h('b', { text: routeLabel(st.route) })));
    out.append(hero);
    if (st.code === 'running' && st.job) {
      out.append(h('div', { class: 'jobbar' }));
    }

    // banners
    if (st.diag && st.diag.status === 'pending') {
      out.append(h('div', { class: 'banner warn' },
        h('h3', { text: 'Разрешите диагностический сбор логов' }),
        h('p', { text: 'Чтобы разобраться, почему агент «Связь Есть?» работает не так, как задумано, поддержке нужны технические логи с этого компьютера.' }),
        st.diag.reason ? h('p', { text: 'Причина запроса: ' + st.diag.reason }) : null,
        h('p', { text: 'В логах — работа агента: модем и сети, связь с сервером, шаги заданий, ошибки. Проверяемые адреса, ваши файлы и другие программы не собираются. Остановить сбор можно в любой момент (Ещё → Диагностика).' }),
        h('div', { class: 'actions' },
          h('button', { class: 'btn ghost small', type: 'button', text: 'Отклонить', onclick: () => diagDecide(false) }),
          h('button', { class: 'btn small', type: 'button', text: 'Разрешить', onclick: () => diagDecide(true) }))));
    } else if (st.diag_on) {
      out.append(h('div', { class: 'banner warn' },
        h('h3', { text: 'Идёт сбор логов для поддержки' }),
        h('p', { text: 'Поддержка выключит его, когда закончит диагностику.' }),
        h('div', { class: 'actions' }, h('button', { class: 'btn ghost small', type: 'button', text: 'Остановить сбор логов', onclick: diagStop }))));
    }
    if (st.gps && !dismissed('gps' + st.gps.id) && Date.now() - new Date(st.gps.at).getTime() < 24 * 3600e3) {
      out.append(h('div', { class: 'banner' },
        h('h3', { text: 'Поддержка запросила проверку местоположения' }),
        h('p', { text: 'На компьютере проверка местоположения недоступна — нет GPS. Поддержке отправлен отказ с этой причиной.' }),
        st.gps.reason ? h('p', { text: 'Причина запроса: ' + st.gps.reason }) : null,
        h('div', { class: 'actions' }, h('button', { class: 'btn ghost small', type: 'button', text: 'Понятно', onclick: () => { dismiss('gps' + st.gps.id); lastKey = ''; render(); } }))));
    }
    if (st.code === 'app_outdated' || P.app_outdated) {
      const minv = st.min_app_version || P.min_app_version;
      const url = st.update_url || P.update_url;
      out.append(h('div', { class: 'banner bad' },
        h('h3', { text: 'Нужно обновление' }),
        h('p', { text: 'Эта версия агента больше не поддерживается' + (minv ? ' (нужна ' + minv + ' или новее)' : '') + '. Пока вы не обновите агент, задания назначаться не будут.' }),
        url ? h('div', { class: 'actions' }, h('a', { class: 'btn small', href: url, target: '_blank', rel: 'noopener', text: 'Скачать обновление' })) : null));
    }
    if (P.moderation_status && P.moderation_status !== 'approved') {
      out.append(h('div', { class: 'banner warn' },
        h('p', { text: P.moderation_status === 'rejected'
          ? 'Заявка партнёра отклонена' + (P.moderation_reason ? ': ' + P.moderation_reason : '') + '. Исправьте данные и подайте заявку заново в Telegram-боте «Связь Есть?».'
          : 'Ваш аккаунт на модерации: мы проверяем заявку партнёра (страна, регион и оператор). Пока её не одобрят, задания не приходят. Результат придёт в Telegram-бот «Связь Есть?».' })));
    }

    // modem
    const m = st.modem;
    if (m) {
      out.append(section('Модем', [
        row('Модель', m.model || '—'),
        // 1.0.5: unknown fields are hidden (a phone over USB tells nothing about them)
        m.operator ? row('Оператор', m.operator + (m.operator_by_ip ? ' (по IP мобильной сети)' : '')) : null,
        m.tech_label ? row('Сеть', m.tech_label) : null,
        m.signal >= 0 ? row('Сигнал', h('div', { class: 'value' }, signalBars(m.signal), m.signal + '%')) : null,
        m.source === 'tether' && !m.operator ? row('Оператор и сигнал', 'телефон по USB их не сообщает — смотрите в самом телефоне') : null,
        row('Мобильный интернет', m.connected ? 'Подключён' : 'Нет подключения', { cls: m.connected ? 'c-ok' : 'c-bad' }),
        row('Интерфейс', m.interface + (m.manual ? ' (вручную)' : ''), { mono: true }),
        m.country ? row(m.country_by_ip ? 'Страна (по IP)' : 'Страна SIM', m.country.toUpperCase()) : null,
      ]));
    } else {
      out.append(section('Модем', [row('Модем не найден', '', { cls: 'c-bad' })],
        'Подключите USB-модем (Huawei HiLink, модем с ModemManager) или телефон в режиме USB-модема. Если модем подключён через другой интерфейс — выберите его в «Ещё → Модем».'));
    }

    // whitelist
    const wl = st.whitelist || {};
    const wlCls = { active: 'c-ok', partial: 'c-warn', inactive: 'c-warn', no_internet: 'c-bad', no_dns: 'c-bad' }[wl.verdict] || '';
    out.append(section('Белые списки', [
      row('Режим у оператора', wl.label || 'ещё не замерялся', { cls: wlCls }),
      row('Замер', wl.at ? ago(wl.at) : '—'),
      row('Задания только при БС', wl.required ? 'Да' : 'Нет'),
    ]));

    if (P.level) out.append(levelCard(P.level));

    if (S.profile) {
      const rows = [row('Доступно', money(P.available_balance ?? P.balance), { cls: 'c-ok' })];
      if (Number(P.held_balance) > 0) rows.push(row('В холде (' + (P.hold_days || 0) + ' дн.)', money(P.held_balance)));
      rows.push(row('Заработано всего', money(P.total_earned)),
        row('Заданий выполнено', String(P.total_jobs_completed ?? '—')),
        row('Трафика использовано', traffic(P.traffic_bytes_total)));
      out.append(section('Баланс', rows, 'Вывод — в Telegram-боте «Связь Есть?».'));
      const pend = P.pending_application;
      const show = pend && pend.kind !== 'change' ? pend : P;
      if (show.operator_label || show.region || pend) {
        let note = null;
        if (pend && pend.kind !== 'change') note = 'Эти данные вы указали в заявке — мы их проверяем. Задания начнут приходить после одобрения.';
        else if (pend) note = 'На модерации новые данные. Пока заявку не одобрят, задания приходят по текущим.';
        out.append(section(pend && pend.kind !== 'change' ? 'Заявка на модерации' : 'Партнёр', [
          row('Оператор', show.operator_label || '—'),
          row('Регион', show.region || '—'),
          show.federal_district_label ? row('Федеральный округ', show.federal_district_label) : null,
        ], note));
      }
    } else {
      out.append(h('div', { class: 'loading' }, h('span', { class: 'spinner small' })));
    }
    out.append(h('p', { class: 'foot', text: 'Проверки идут только через мобильный модем, связь с сервером — через другую сеть (Ethernet / Wi-Fi), модем — запасной путь.' }));
    return out;
  }

  function levelCard(L) {
    const next = L.next_min;
    const frac = next == null ? 1 : Math.max(0, Math.min(1, (L.points - L.level_min) / Math.max(1, next - L.level_min)));
    const pct = (v) => (Number.isInteger(v) ? v : Number(v).toFixed(1)) + '%';
    const bar = h('i', { class: next == null ? 'max' : '' });
    requestAnimationFrame(() => requestAnimationFrame(() => { bar.style.width = (frac * 100).toFixed(1) + '%'; }));
    const card = h('div', { class: 'level' },
      h('div', { class: 'level-top' },
        h('div', { class: 'lvl-badge', text: String(L.level) }),
        h('div', null, h('div', { class: 'lvl-name', text: 'Уровень ' + L.level }),
          h('div', { class: 'lvl-share', text: 'Доля ' + pct(L.share) + ' с каждой проверки' })),
        L.streak_days > 0 ? h('div', { class: 'streak' + (L.streak_today ? ' today' : ''), title: L.streak_today ? 'Сегодня уже засчитано' : 'Будьте на связи сегодня, чтобы не прервать серию', text: '🔥 ' + L.streak_days }) : null),
      h('div', { class: 'progress' }, bar),
      h('div', { class: 'lvl-pts' }, h('span', { text: L.points + ' очк.' }),
        h('span', { text: next == null ? 'максимальный уровень' : 'до уровня ' + (L.level + 1) + ': ' + next + (L.next_share != null ? ' (доля ' + pct(L.next_share) + ')' : '') })),
      (L.achievements || []).length ? h('div', { class: 'ach' }, L.achievements.map((a) => {
        const got = !!a.earned_at;
        return h('div', { class: 'medal' + (got ? ' got' : ''), title: a.title, onclick: () => achievementModal(a) },
          h('div', { class: 'ring', text: a.emoji || '🏅' }), h('div', { class: 't', text: a.title }));
      })) : null,
      h('div', { class: 'lvl-note', text: 'Уровень растёт, пока агент на связи (больше — при белых списках), и за каждое выполненное задание.' }));
    return h('div', { class: 'section' }, h('div', { class: 'section-h', text: 'Уровень' }), card);
  }

  function achievementModal(a) {
    const got = !!a.earned_at;
    let close;
    close = modal([
      h('div', { class: 'medal-big', text: a.emoji || '🏅' }),
      h('h3', { text: a.title }),
      h('p', { text: a.desc || '' }),
      h('p', { class: got ? 'c-ok' : '', text: got ? 'Получено ' + new Date(a.earned_at).toLocaleDateString('ru-RU') : 'Ещё не получено' }),
      h('div', { class: 'actions' }, h('button', { class: 'btn small', type: 'button', text: 'Закрыть', onclick: () => close() })),
    ]);
  }

  const seenKey = 'seen_achievements';
  function checkAchievements() {
    const L = S && S.profile && S.profile.level;
    if (!L) return;
    const got = (L.achievements || []).filter((a) => a.earned_at).map((a) => a.code);
    const prev = localStorage.getItem(seenKey);
    localStorage.setItem(seenKey, got.join(','));
    const prevLevel = Number(localStorage.getItem('seen_level') || 0);
    localStorage.setItem('seen_level', String(L.level));
    if (prev === null) return;
    const before = new Set(prev.split(','));
    const fresh = (L.achievements || []).find((a) => a.earned_at && !before.has(a.code));
    if (fresh) toast('Новое достижение: «' + fresh.title + '»');
    else if (prevLevel && L.level > prevLevel) toast('Новый уровень: ' + L.level);
  }

  const dismissed = (k) => localStorage.getItem('dismiss_' + k) === '1';
  const dismiss = (k) => localStorage.setItem('dismiss_' + k, '1');

  async function diagDecide(accept) {
    try {
      const r = await api('/ui/api/diag/decision', { accept });
      toast(r.status === 'accepted' ? 'Сбор логов разрешён. Остановить — в «Ещё»' : 'Сбор логов отклонён');
    } catch (e) { toast(e.message); }
  }
  async function diagStop() {
    if (!(await confirmBox('Остановить сбор логов?', 'Поддержка перестанет получать логи с этого компьютера.', 'Остановить', true))) return;
    try { await api('/ui/api/diag/stop', {}); toast('Сбор логов остановлен'); } catch (e) { toast(e.message); }
  }

  // ---------- history ----------
  async function loadHistory() {
    try { history = await api('/ui/api/history'); } catch (e) { history = { error: e.message }; }
    if (tab === 'history') render(false);
  }
  const typeLabels = { vpn_key: 'VPN', ip: 'Домен/IP', domain: 'Домен/IP', subnet: 'Подсеть /24' };
  const statusLabels = { failed: 'Не удалось', expired: 'Просрочено', pending: 'В очереди', assigned: 'Назначено', running: 'Выполняется',
    released: 'Возвращено', lost: 'Закрыто сервером', undelivered: 'Не доставлено' };
  function dayTitle(d) {
    const today = new Date(); today.setHours(0, 0, 0, 0);
    const day = new Date(d); day.setHours(0, 0, 0, 0);
    const diff = Math.round((today - day) / 86400e3);
    if (diff === 0) return 'Сегодня';
    if (diff === 1) return 'Вчера';
    return two(day.getDate()) + '.' + two(day.getMonth() + 1) + '.' + day.getFullYear();
  }
  function viewHistory() {
    if (!history) { loadHistory(); return h('div', { class: 'loading' }, h('span', { class: 'spinner' })); }
    let items = [];
    let note = null;
    if (history.jobs) {
      items = history.jobs.map((j) => ({ type: j.resource_type, status: j.status, reward: j.reward_amount, at: j.completed_at || j.created_at }));
    } else if (history.local && history.local.length) {
      note = (history.error || 'Сервер недоступен') + ' — показаны задания этого компьютера.';
      items = history.local.map((j) => ({ type: j.type, status: j.status, reward: null, at: j.at }));
    } else if (history.error) {
      return h('div', { class: 'empty', text: history.error });
    }
    if (!items.length) return h('div', { class: 'empty', text: 'Пока нет заданий' });
    const groups = [];
    const byTitle = {};
    for (const it of items) {
      const t = dayTitle(it.at);
      if (!byTitle[t]) { byTitle[t] = []; groups.push([t, byTitle[t]]); }
      byTitle[t].push(it);
    }
    const out = h('div');
    if (note) out.append(h('div', { class: 'notice', text: note }));
    for (const [title, list] of groups) {
      out.append(section(title, list.map((it) => {
        const d = new Date(it.at);
        let text, cls;
        if (it.status === 'completed') {
          text = it.reward != null ? '+' + Number(it.reward).toFixed(4) + ' USD' : 'Выполнено';
          cls = 'c-ok';
        } else if (it.status === 'failed' || it.status === 'undelivered') {
          text = statusLabels[it.status]; cls = 'c-bad';
        } else { text = statusLabels[it.status] || it.status; cls = ''; }
        return row(typeLabels[it.type] || it.type, h('div', { class: 'value trail ' + cls, text }), { sub: two(d.getHours()) + ':' + two(d.getMinutes()) });
      })));
    }
    return out;
  }

  // ---------- more / settings ----------
  async function loadModems(refresh) {
    try { modems = await api(refresh ? '/ui/api/modems/refresh' : '/ui/api/modems', refresh ? {} : undefined); } catch (e) { toast(e.message); }
    if (tab === 'more') render(false);
  }
  const attestLabel = (s) => ({ desktop: 'Привязан', verified: 'Подтверждён', unsupported: 'Сервер не проверяет', failed: 'Не принят', error: 'Ошибка' }[s] || (s ? s : 'Ещё не привязан'));

  function viewMore() {
    const st = S.status;
    const A = S.about;
    const out = h('div');
    out.append(section(null, [
      row('ID партнёра', st.executor_id || '—', { mono: true }),
      row('Версия агента', A.version),
      row('Система', A.os),
      row('Компьютер', A.model),
      row('Xray-core', [A.xray, A.xray_next].filter(Boolean).join(' / ') || 'не найден'),
      row('Ключ устройства', attestLabel(st.attest), { cls: st.attest === 'desktop' || st.attest === 'verified' ? 'c-ok' : (st.attest === 'failed' ? 'c-bad' : '') }),
    ]));

    // modem choice
    const sel = h('select', { class: 'select', id: 'modem-sel' });
    sel.append(h('option', { value: '', text: 'Автоматически' }));
    const opts = (modems && modems.options) || [];
    const cur = (modems && modems.selected) || A.modem_iface || '';
    let hasCur = cur === '';
    for (const o of opts) {
      const op = h('option', { value: o.value, text: o.label });
      if (o.value === cur) { op.selected = true; hasCur = true; }
      sel.append(op);
    }
    if (!hasCur) sel.append(h('option', { value: cur, text: cur + ' (не найден)', selected: true }));
    sel.addEventListener('change', async () => {
      try { await api('/ui/api/settings', { modem_iface: sel.value }); toast(sel.value ? 'Модем: ' + sel.value : 'Модем выбирается автоматически'); loadModems(); } catch (e) { toast(e.message); }
    });
    if (A.modem_override) sel.disabled = true;
    const m = st.modem;
    out.append(section('Модем', [
      row('Интерфейс модема', sel),
      row('Сейчас используется', m ? m.interface + (m.connected ? '' : ' (нет сети)') : 'нет', { cls: m && m.connected ? '' : 'c-bad', mono: !!m }),
      row('Найти модемы заново', undefined, { kind: 'accent', onclick: () => { toast('Поиск модемов…'); loadModems(true); } }),
    ], A.modem_override ? 'Модем задан при запуске (--modem-iface ' + A.modem_override + ') — выбор здесь не действует.'
      : modems && modems.error ? 'Поиск модемов: ' + modems.error : 'Автоматически — первый подключённый модем. Задания не берутся, пока модема нет.'));

    out.append(section('Связь с сервером', [row('Сейчас через', routeLabel(st.route))],
      'Связь с сервером идёт мимо модема (Ethernet / Wi-Fi), модем — только запасной путь.'));

    const auto = h('label', { class: 'switch' }, h('input', { type: 'checkbox', checked: A.autostart }), h('span'));
    auto.querySelector('input').addEventListener('change', async (e) => {
      try { await api('/ui/api/settings', { autostart: e.target.checked }); toast(e.target.checked ? 'Автозапуск включён' : 'Автозапуск выключен'); } catch (ex) { e.target.checked = !e.target.checked; toast(ex.message); }
    });
    out.append(section('Служба', [
      row('Запускать вместе с системой', auto),
      row('Работает с', new Date(st.started).toLocaleString('ru-RU')),
      row('Выполнено за всё время', String(st.jobs_total || 0)),
      row('Перезапустить службу', undefined, { kind: 'accent', onclick: async () => {
        if (!(await confirmBox('Перезапустить службу?', 'Текущая проверка, если она идёт, будет прервана и вернётся в очередь.', 'Перезапустить'))) return;
        try { await api('/ui/api/restart', {}); toast('Служба перезапускается…'); } catch (e) { toast(e.message); }
      } }),
    ]));

    out.append(section('Диагностика', [
      row('Сбор логов для поддержки', st.diag_on ? 'включён' : (st.diag && st.diag.status === 'pending' ? 'ждёт ответа' : 'выключен'), { cls: st.diag_on || (st.diag && st.diag.status === 'pending') ? 'c-warn' : '' }),
      st.diag && st.diag.status === 'pending' ? row('Ответить на запрос', undefined, { kind: 'accent', chev: true, onclick: () => setTab('home') }) : null,
      st.diag_on ? row('Остановить сбор логов', undefined, { kind: 'danger', onclick: diagStop }) : null,
      row('Журнал службы', undefined, { chev: true, onclick: journalModal }),
    ], 'Логи уходят поддержке только с вашего согласия; проверяемые адреса в них не попадают.'));

    const pageRows = [row('Адрес', A.listen + (A.loopback_only ? ' (только этот компьютер)' : ''), { mono: true }),
      row('Пароль страницы', A.password_set ? 'свой' : (A.loopback_only ? 'не задан' : 'код настройки'), { cls: A.password_set ? 'c-ok' : '' }),
      row(A.password_set ? 'Сменить пароль' : 'Задать свой пароль', undefined, { kind: 'accent', chev: true, onclick: passwordModal })];
    if (A.password_set || !A.loopback_only) pageRows.push(row('Выйти со страницы', undefined, { onclick: async () => { await api('/ui/api/auth/logout', {}).catch(() => {}); checkAuth(); } }));
    out.append(section('Страница агента', pageRows));

    out.append(section(null, [row('Бот «Связь Есть?»', undefined, { chev: true, href: A.bot_url })]));
    out.append(section(null, [row('Выйти из аккаунта', undefined, { kind: 'danger', onclick: async () => {
      if (!(await confirmBox('Выйти из аккаунта?', 'Задания перестанут приходить на этот компьютер, пока вы не войдёте снова.', 'Выйти', true))) return;
      try { await api('/ui/api/logout', {}); } catch (e) { toast(e.message); }
    } })]));
    out.append(h('p', { class: 'foot', text: 'Данные агента: ' + A.data_dir + ' (' + A.protection + ')' }));
    return out;
  }

  async function journalModal() {
    const list = h('div', { class: 'journal' }, h('span', { class: 'spinner small' }));
    const load = async () => {
      try {
        const r = await api('/ui/api/journal');
        list.replaceChildren(...(r.lines || []).map((l) => {
          const d = new Date(l.t);
          return h('div', { class: l.level === 'warn' ? 'warn' : '' },
            h('span', { class: 't', text: two(d.getDate()) + '.' + two(d.getMonth() + 1) + ' ' + two(d.getHours()) + ':' + two(d.getMinutes()) + ':' + two(d.getSeconds()) }), l.msg);
        }));
        if (!(r.lines || []).length) list.textContent = 'Журнал пуст';
        list.scrollTop = list.scrollHeight;
      } catch (e) { list.textContent = e.message; }
    };
    let close;
    close = modal([h('h3', { text: 'Журнал службы' }), list,
      h('div', { class: 'actions' },
        h('button', { class: 'btn ghost small', type: 'button', text: 'Обновить', onclick: load }),
        h('button', { class: 'btn small', type: 'button', text: 'Закрыть', onclick: () => close() }))], { wide: true });
    load();
  }

  function passwordModal() {
    const set = S.about.password_set;
    const old = set ? h('input', { class: 'input', type: 'password', placeholder: 'Текущий пароль', autocomplete: 'current-password' }) : null;
    const p1 = h('input', { class: 'input', type: 'password', placeholder: 'Новый пароль (от 8 символов)', autocomplete: 'new-password' });
    const p2 = h('input', { class: 'input', type: 'password', placeholder: 'Ещё раз', autocomplete: 'new-password' });
    const err = h('div', { class: 'err' });
    let close;
    const form = h('form', { class: 'stack', onsubmit: async (e) => {
      e.preventDefault();
      err.textContent = '';
      if (p1.value !== p2.value) { err.textContent = 'Пароли не совпадают'; return; }
      try {
        await api('/ui/api/auth/password', { old: old ? old.value : '', new: p1.value });
        close(); toast('Пароль страницы сохранён'); checkAuth();
      } catch (ex) { err.textContent = ex.message; }
    } }, old, p1, p2, err,
    h('div', { class: 'actions' },
      h('button', { class: 'btn ghost small', type: 'button', text: 'Отмена', onclick: () => close() }),
      h('button', { class: 'btn small', type: 'submit', text: 'Сохранить' })));
    close = modal([h('h3', { text: set ? 'Сменить пароль страницы' : 'Задать свой пароль страницы' }),
      h('p', { text: set ? 'После смены пароля все открытые сеансы страницы закроются.'
        : 'Вместо кода настройки будет ваш пароль; код перестанет действовать, открытые сеансы страницы закроются.' }), form]);
    (old || p1).focus();
  }

  // ---------- 1 s ticker: countdown & job timer without re-rendering ----------
  setInterval(() => {
    const left = document.getElementById('login-left');
    if (left && left.dataset.exp) {
      const s = Math.max(0, Math.round((new Date(left.dataset.exp).getTime() - Date.now()) / 1000));
      left.textContent = 'Ссылка действует ещё ' + Math.floor(s / 60) + ':' + two(s % 60);
    }
    const sub = document.querySelector('.tone-ok .subtitle');
    if (sub && S && S.status.code === 'running' && S.status.job) sub.textContent = S.status.text + ' · ' + dur(S.status.job.since);
  }, 1000);

  checkAuth();
})();
