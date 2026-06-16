// app.js — page-level glue that can't be inline (CSP blocks
// onsubmit= / onclick= attributes). Loaded by layout.html so all
// pages get the same behaviour.
(function () {
  'use strict';

  // Confirm-on-submit: any <form class="confirm-delete"> asks the
  // user before sending. data-confirm is the message.
  document.addEventListener('submit', function (e) {
    const form = e.target;
    if (form && form.classList && form.classList.contains('confirm-delete')) {
      const msg = form.getAttribute('data-confirm') || '确认执行此操作？';
      if (!window.confirm(msg)) {
        e.preventDefault();
      }
    }
  });
})();
