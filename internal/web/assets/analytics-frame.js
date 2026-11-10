/*
 * Third-party analytics runs here, never in the application document.
 *
 * This page is loaded by the console inside:
 *
 *   <iframe sandbox="allow-scripts" src="/analytics-frame.html?provider=..&site=..&src=..">
 *
 * The sandbox has no allow-same-origin, so this document gets an opaque
 * origin: it cannot read the console DOM, cannot read cookies, and a request
 * to /api/... is cross-origin and uncredentialed. Only the analytics script
 * origin is allowed by this page's Content-Security-Policy, so even a
 * compromised vendor script cannot reach the application's API.
 *
 * Everything from the query string is only ever placed into data attributes
 * on the vendor <script> tag; nothing from the URL becomes markup.
 */
(function () {
  'use strict';

  function param(name) {
    try {
      return new URLSearchParams(location.search).get(name) || '';
    } catch (e) {
      return '';
    }
  }

  var src = param('src');
  if (!src) return;

  // Only absolute https URLs are accepted. The server independently validates
  // the stored value and its CSP would refuse anything else; this is defence
  // in depth so the frame never even attempts a refused load.
  if (!/^https:\/\/[^/]/i.test(src)) return;

  var provider = param('provider');
  var site = param('site');

  var script = document.createElement('script');
  script.src = src;
  script.async = true;
  script.defer = true;
  script.setAttribute('data-tiller-analytics', '1');
  if (provider === 'umami' && site) script.setAttribute('data-website-id', site);
  else if (provider === 'plausible' && site) script.setAttribute('data-domain', site);
  document.head.appendChild(script);
})();
