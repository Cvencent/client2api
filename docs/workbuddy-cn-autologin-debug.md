# WorkBuddy CN auto-login debug notes

Date: 2026-10-13

## Symptom

After the CN SMS code is submitted, the SPA ends on `/login/undefined`
(`?platform=admin&state=0`) and the plugin session times out. `/login/undefined`
is just the catch-all route `* -> /login`; the vendor login form renders there,
so it is a destination the SPA navigated to, not a crashed page.

## Confirmed

- CN only; the global realm is untouched.
- The `/v2/plugin/auth/state` call returns `authUrl = /login?platform=workbuddy&state=...`.
- The CN SPA computes an origin-relative iframe src; its router target becomes
  `/login/<return url>` where `<return url>` is `sessionStorage["login-return"]`
  (or a default). When that value is `undefined`, the path is `/login/undefined`.
- The hidden `#phone-iframe` publishes `contentWindow.__ONEID__.loginUrl` within
  a couple hundred ms of the phone tab being shown, and the value is valid.
- The OneID account-selection success callback reads `await $1(p.loginUrl)` where
  `$1 = GET <loginUrl>&from_oneid_login=true`. That GET returns JSON
  `{code:0, state, redirect_uri}`.
- A valid `loginUrl + "&from_oneid_login=true"` GET returns HTTP 200 JSON.

## Attempted (do not repeat)

1. v0.1.54: wait for `__ONEID__.loginUrl`, then replay the iframe `load` event
   before SMS. Real CN run still failed.
2. Local only: replay `load` again right before the account click, plus a 300 ms
   settle. Real CN run still failed.
3. Local only: iterate every `#phone-iframe` instead of `getElementById`, and
   observe a 1.5 s stability window for a remount.
4. No-SMS CDP probes (state-probe, timeline probe). The iframe is stable, the
   `loginUrl` appears once, and the load replay updates the React state. The
   remount hypothesis is disproven.

## Excluded or weak

- Global realm path.
- Missing `loginUrl` at initial phone-form time.
- SMS field / submit-button error.
- Repeatedly renting real numbers.

## Next

1. Capture the ground-truth SPA navigation + request timeline around the SMS
   submit using the module's own browser (attach CDP to the launched browser).
2. Find what writes `sessionStorage["login-return"]` = `undefined` (the default
   is a value, so something is expected to have set it).
3. Fix at that boundary, then run tests and at most one real verification.
