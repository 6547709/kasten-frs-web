// app.js — page-level glue that can't be inline (CSP blocks
// onsubmit= / onclick= attributes, and any <script> body that isn't
// a nonce/hashed src). Loaded by layout.html once per page, then
// routes to the right init based on <body data-page="...">.
(function () {
  'use strict';

  const $ = (s, r) => (r || document).querySelector(s);
  const $$ = (s, r) => Array.from((r || document).querySelectorAll(s));

  // ---------- shared ----------

  // Confirm-on-submit: any <form class="confirm-delete"> asks the
  // user before sending. data-confirm is the message.
  document.addEventListener('submit', function (e) {
    const form = e.target;
    if (form && form.classList && form.classList.contains('confirm-delete')) {
      const msg = form.getAttribute('data-confirm') || 'Confirm this action?';
      if (!window.confirm(msg)) {
        e.preventDefault();
      }
    }
  });

  // ---------- /sessions ----------

  function initSessions() {
    if (window.__kfrsCountdown) return;
    window.__kfrsCountdown = true;
    function fmt(ms) {
      if (ms < 0) return '已过期';
      const s = Math.floor(ms / 1000);
      const d = Math.floor(s / 86400),
        h = Math.floor((s % 86400) / 3600),
        m = Math.floor((s % 3600) / 60);
      if (d > 0) return '剩 ' + d + 'd ' + (h < 10 ? '0' : '') + h + 'h';
      return '剩 ' + (h < 10 ? '0' : '') + h + 'h ' + (m < 10 ? '0' : '') + m + 'm';
    }
    function tick() {
      const now = Date.now();
      $$('[data-expiry]').forEach(function (el) {
        const t = Date.parse(el.getAttribute('data-expiry'));
        const ms = t - now;
        const span = el.querySelector('.exp-text');
        if (!span) return;
        if (ms < 0) { el.className = 'badge crit'; span.textContent = '已过期'; return; }
        if (ms < 15 * 60 * 1000) el.className = 'badge crit';
        else if (ms < 60 * 60 * 1000) el.className = 'badge warn';
        else el.className = 'badge';
        span.textContent = fmt(ms);
      });
    }
    tick();
    let id = setInterval(tick, 1000);
    document.addEventListener('visibilitychange', function () {
      if (document.hidden) { clearInterval(id); id = null; }
      else if (!id) { tick(); id = setInterval(tick, 1000); }
    });
  }

  // ---------- /browse (preparing) ----------

  function initBrowsePreparing() {
    const el = document.getElementById('elapsed');
    if (!el) return;
    const start = Date.now();
    setInterval(function () {
      el.textContent = Math.floor((Date.now() - start) / 1000);
    }, 1000);
  }

  // ---------- /wizard ----------

  function initWizard() {
    function applyFilter() {
      const filter = $('#vm-filter');
      if (!filter) return;
      const q = (filter.value || '').toLowerCase();
      let visible = 0;
      $$('#vm-list li').forEach(function (li) {
        if (!li.dataset.vmName) return; // skip empty/loading rows
        const name = li.dataset.vmName.toLowerCase();
        const ns = (li.dataset.vmNs || '').toLowerCase();
        const hit = !q || name.includes(q) || ns.includes(q);
        li.style.display = hit ? '' : 'none';
        if (hit) visible++;
      });
      const c = $('#vm-count');
      if (c) c.textContent = visible + ' / ' + $$('#vm-list li[data-vm-name]').length + ' 个 VM';
    }
    window.__kfrsApplyFilter = applyFilter;

    const filter = $('#vm-filter');
    if (filter) filter.addEventListener('input', applyFilter);

    const nsSel = $('#ns-select');
    if (nsSel) {
      nsSel.addEventListener('change', function () {
        const ns = nsSel.value;
        // clear dependent state
        const vmNs = $('#vm-ns'), vmName = $('#vm-name'), rpName = $('#rp-name'),
          pvcFields = $('#pvc-fields'), submit = $('#wizard-submit'),
          rpList = $('#rp-list'), volList = $('#vol-list');
        if (vmNs) vmNs.value = '';
        if (vmName) vmName.value = '';
        if (rpName) rpName.value = '';
        if (pvcFields) pvcFields.innerHTML = '';
        if (submit) submit.disabled = true;
        if (rpList) rpList.innerHTML = '<p class="empty">从左侧选一个 VM</p>';
        if (volList) volList.innerHTML = '<p class="empty">从中间选一个还原点</p>';
        if (filter) filter.value = '';
        const list = $('#vm-list');
        if (!list) return;
        list.innerHTML = '<li class="empty">载入中…</li>';
        // htmx 1.x's hx-trigger="load" fires only once. Re-setting
        // hx-get via setAttribute then re-triggering 'load' is fragile
        // — the safer path is to issue a direct htmx.ajax and let it
        // swap into the same target with innerHTML.
        const url = ns ? '/wizard/vms?ns=' + encodeURIComponent(ns) : '/wizard/vms';
        if (window.htmx) {
          htmx.ajax('GET', url, { target: '#vm-list', swap: 'innerHTML' });
        } else {
          // Fallback for tests or js-disabled environments — at least
          // navigate the browser to a URL that renders the right list.
          window.location.href = '/wizard?ns=' + encodeURIComponent(ns);
        }
      });
    }

    // Click on a VM row → fetch restore points
    document.body.addEventListener('click', function (e) {
      const li = e.target.closest('#vm-list li');
      if (!li) return;
      $$('#vm-list li').forEach(function (x) { x.classList.remove('selected'); });
      li.classList.add('selected');
      const ns = li.dataset.vmNs, name = li.dataset.vmName;
      const vmNs = $('#vm-ns'), vmName = $('#vm-name'), rpName = $('#rp-name'),
        pvcFields = $('#pvc-fields'), submit = $('#wizard-submit');
      if (vmNs) vmNs.value = ns;
      if (vmName) vmName.value = name;
      if (rpName) rpName.value = '';
      if (pvcFields) pvcFields.innerHTML = '';
      if (submit) submit.disabled = true;
      if (window.htmx) {
        htmx.ajax('GET', '/wizard/vms/' + encodeURIComponent(ns) + '/' + encodeURIComponent(name) + '/restorepoints',
          { target: '#rp-list', swap: 'innerHTML' });
      }
    });

    // Restore-point row click → fetch volumes. We bind on every
    // #rp-list htmx swap. Stashing the handler on the element lets
    // us remove the old binding before adding the new one — without
    // that, a single click would fire N handlers from accumulated
    // previous renders.

    // Single source of truth for "should the wizard-submit be
    // enabled?". Called after every vol-list swap and on every
    // checkbox change. Avoids the previous bug where the disabled
    // flag was set once after htmx:afterRequest and then never
    // updated when the user toggled checkboxes.
    function enableSubmitIfVolumesPresent() {
      const submit = $('#wizard-submit');
      const pvcFields = $('#pvc-fields');
      if (!submit) return;
      const cbs = $$('#vol-list input[name=pvcNames]');
      const checked = cbs.filter(function (x) { return x.checked; });
      submit.disabled = checked.length === 0;
      if (pvcFields) {
        pvcFields.innerHTML = checked.map(function (v) {
          return '<input type="hidden" name="pvcNames" value="' + v.value + '">';
        }).join('');
      }
    }
    window.__kfrsRefreshVolumes = enableSubmitIfVolumesPresent;

    function bindRpClick(list) {
      const handler = function (e2) {
        const li = e2.target.closest('li');
        if (!li) return;
        $$('#rp-list li').forEach(function (x) { x.classList.remove('selected'); });
        li.classList.add('selected');
        const name = li.dataset.rpName;
        const rpName = $('#rp-name');
        if (rpName) rpName.value = name;
        if (window.htmx) {
          htmx.ajax('GET', '/wizard/rps/' + encodeURIComponent(li.dataset.rpNs) + '/' + encodeURIComponent(name) + '/details',
            { target: '#vol-list', swap: 'innerHTML' });
        }
      };
      if (list._rpClick) list.removeEventListener('click', list._rpClick);
      list._rpClick = handler;
      list.addEventListener('click', handler);
    }
    const rpList = $('#rp-list');
    if (rpList) bindRpClick(rpList);

    // Delegated change listener on #vol-list: every checkbox the user
    // toggles should refresh the hidden pvcNames mirror + submit
    // disabled flag. Because htmx swaps innerHTML on #vol-list, the
    // checkboxes themselves are new each time, so a delegated listener
    // on the stable parent is the only reliable hook.
    const volList = $('#vol-list');
    if (volList) {
      volList.addEventListener('change', function (ev) {
        if (ev && ev.target && ev.target.name === 'pvcNames') {
          enableSubmitIfVolumesPresent();
        }
      });
    }

    document.body.addEventListener('htmx:afterRequest', function (e) {
      if (e.target.id === 'vm-list') {
        if (typeof window.__kfrsApplyFilter === 'function') window.__kfrsApplyFilter();
      }
      if (e.target.id === 'rp-list') {
        // htmx replaced the #rp-list node, so the binding above is on
        // the OLD node. Rebind on the new one now that the swap is
        // done.
        bindRpClick(e.target);
      }
      if (e.target.id === 'vol-list') {
        enableSubmitIfVolumesPresent();
      }
    });
  }

  // ---------- router ----------

  // app.js is loaded synchronously from <head>, before <body> has
  // been parsed. Read data-page off body too early and we get '';
  // every init branch would then no-op and the page would silently
  // stop working (countdown never updates, VM clicks never fetch RP
  // list, etc). Wait for DOMContentLoaded so data-page + the wizard
  // panel nodes are in place when init runs.
  function route() {
    const page = (document.body && document.body.getAttribute('data-page')) || '';
    if (page === 'sessions_body') initSessions();
    else if (page === 'wizard_body') initWizard();
    else if (page === 'browse_preparing_body') initBrowsePreparing();
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', route);
  } else {
    route();
  }
})();
