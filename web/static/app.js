// app.js — page-level glue that can't be inline (CSP blocks
// onsubmit= / onclick= attributes, and any <script> body that isn't
// a nonce/hashed src). Loaded by layout.html once per page, then
// routes to the right init based on <body data-page="...">.
(function () {
  'use strict';

  const $ = (s, r) => (r || document).querySelector(s);
  const $$ = (s, r) => Array.from((r || document).querySelectorAll(s));

  // ---------- shared: in-app confirm modal ----------

  // A single reusable modal replaces the browser-native window.confirm
  // for any <form class="confirm-delete">. The native dialog is ugly,
  // unbranded, and visually jarring; this one matches the Kasten theme,
  // animates in, traps focus, and is keyboard-accessible (Esc cancels,
  // Enter confirms). Built lazily on first use and reused thereafter.
  let modalEl = null;
  let modalResolve = null;

  function buildModal() {
    const overlay = document.createElement('div');
    overlay.className = 'kfrs-modal-overlay';
    overlay.setAttribute('role', 'dialog');
    overlay.setAttribute('aria-modal', 'true');
    overlay.setAttribute('aria-labelledby', 'kfrs-modal-title');
    overlay.innerHTML =
      '<div class="kfrs-modal">' +
      '  <h2 id="kfrs-modal-title" class="kfrs-modal-title">Confirm</h2>' +
      '  <p class="kfrs-modal-msg"></p>' +
      '  <div class="kfrs-modal-actions">' +
      '    <button type="button" class="kfrs-modal-cancel">Cancel</button>' +
      '    <button type="button" class="kfrs-modal-ok danger">Delete</button>' +
      '  </div>' +
      '</div>';
    document.body.appendChild(overlay);

    const cancel = overlay.querySelector('.kfrs-modal-cancel');
    const ok = overlay.querySelector('.kfrs-modal-ok');
    function close(result) {
      overlay.classList.remove('open');
      document.removeEventListener('keydown', onKey);
      const r = modalResolve;
      modalResolve = null;
      // Wait for the fade-out transition before hiding from layout.
      setTimeout(function () { overlay.style.display = 'none'; }, 160);
      if (r) r(result);
    }
    function onKey(ev) {
      if (ev.key === 'Escape') { ev.preventDefault(); close(false); }
      else if (ev.key === 'Enter') { ev.preventDefault(); close(true); }
    }
    cancel.addEventListener('click', function () { close(false); });
    ok.addEventListener('click', function () { close(true); });
    overlay.addEventListener('click', function (ev) {
      if (ev.target === overlay) close(false); // click backdrop = cancel
    });
    overlay._close = close;
    overlay._onKey = onKey;
    return overlay;
  }

  // confirmModal(message, opts) → Promise<boolean>. opts.okLabel and
  // opts.title customise the dialog; defaults suit a destructive delete.
  function confirmModal(message, opts) {
    opts = opts || {};
    if (!modalEl) modalEl = buildModal();
    const titleEl = modalEl.querySelector('.kfrs-modal-title');
    const msgEl = modalEl.querySelector('.kfrs-modal-msg');
    const okEl = modalEl.querySelector('.kfrs-modal-ok');
    titleEl.textContent = opts.title || 'Confirm deletion';
    msgEl.textContent = message;
    okEl.textContent = opts.okLabel || 'Delete';
    modalEl.style.display = 'flex';
    // Force reflow so the .open transition actually animates.
    void modalEl.offsetWidth;
    modalEl.classList.add('open');
    document.addEventListener('keydown', modalEl._onKey);
    // Focus the safe (cancel) action by default for destructive dialogs.
    modalEl.querySelector('.kfrs-modal-cancel').focus();
    return new Promise(function (resolve) { modalResolve = resolve; });
  }
  window.__kfrsConfirm = confirmModal;

  // Confirm-on-submit: any <form class="confirm-delete"> routes through
  // the in-app modal instead of window.confirm. We intercept submit,
  // hold it, then re-submit programmatically once the user confirms.
  document.addEventListener('submit', function (e) {
    const form = e.target;
    if (!(form && form.classList && form.classList.contains('confirm-delete'))) return;
    if (form._kfrsConfirmed) { form._kfrsConfirmed = false; return; } // allow the re-submit through
    e.preventDefault();
    const msg = form.getAttribute('data-confirm') || 'Are you sure you want to proceed?';
    confirmModal(msg).then(function (ok) {
      if (!ok) return;
      form._kfrsConfirmed = true;
      if (typeof form.requestSubmit === 'function') form.requestSubmit();
      else form.submit();
    });
  });

  // ---------- /sessions ----------

  function initSessions() {
    if (window.__kfrsCountdown) return;
    window.__kfrsCountdown = true;
    function fmt(ms) {
      if (ms < 0) return 'expired';
      const s = Math.floor(ms / 1000);
      const d = Math.floor(s / 86400),
        h = Math.floor((s % 86400) / 3600),
        m = Math.floor((s % 3600) / 60);
      // Pad d to 2 digits too. Without it, "9d 03h left" ->
      // "10d 03h left" causes a width jump at the 9d/10d
      // boundary, and the badge shifts horizontally inside the
      // table cell. The CSS min-width on .exp-text absorbs one
      // such jump; the padStart here makes the format byte-for-
      // byte width-stable from "00d 00h left" through
      // "99d 23h left".
      const dStr = String(d).padStart(2, '0');
      if (d > 0) return dStr + 'd ' + (h < 10 ? '0' : '') + h + 'h left';
      return (h < 10 ? '0' : '') + h + 'h ' + (m < 10 ? '0' : '') + m + 'm left';
    }
    function tick() {
      const now = Date.now();
      $$('[data-expiry]').forEach(function (el) {
        const t = Date.parse(el.getAttribute('data-expiry'));
        const ms = t - now;
        const span = el.querySelector('.exp-text');
        if (!span) return;
        if (ms < 0) { el.className = 'badge crit'; span.textContent = 'expired'; return; }
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
    let id = setInterval(function () {
      // Pad to 3 digits so the centered preparing page doesn't
      // reflow as the digit count changes (0->9, 9->10, 10->99,
      // ...). 3 digits is enough for the default 120s
      // HELPER_FRS_WAIT_TIMEOUT; operators with longer timeouts
      // (>999s) get a one-time reflow at the 999->1000 boundary,
      // which is rare enough to not be worth more padding.
      const secs = Math.floor((Date.now() - start) / 1000);
      el.textContent = String(secs).padStart(3, '0');
    }, 1000);
    // Stop ticking when the element leaves the DOM (htmx swaps the
    // preparing wrapper on terminal/ready states) or the page is
    // hidden, so we don't leak a 1s timer for the life of the tab.
    function stop() {
      if (id) { clearInterval(id); id = null; }
    }
    document.addEventListener('visibilitychange', function () {
      if (document.hidden) stop();
    });
    document.body.addEventListener('htmx:afterSwap', function () {
      if (!document.body.contains(el)) stop();
    });
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
      if (c) c.textContent = visible + ' / ' + $$('#vm-list li[data-vm-name]').length + ' VMs';
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
        if (rpList) rpList.innerHTML = '<p class="empty">Select a VM on the left</p>';
        if (volList) volList.innerHTML = '<p class="empty">Select a RestorePoint in the middle column</p>';
        if (filter) filter.value = '';
        const list = $('#vm-list');
        if (!list) return;
        list.innerHTML = '<li class="empty">Loading…</li>';
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
        // Build the hidden pvcNames mirror with createElement instead
        // of innerHTML string concatenation. PVC names come from the
        // K8s API (and ultimately the FRS server) and could in theory
        // contain markup; setting .value on a real element treats the
        // string as data, never as HTML, so it can't break out of the
        // attribute and inject script.
        pvcFields.textContent = '';
        checked.forEach(function (v) {
          var inp = document.createElement('input');
          inp.type = 'hidden';
          inp.name = 'pvcNames';
          inp.value = v.value;
          pvcFields.appendChild(inp);
        });
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
