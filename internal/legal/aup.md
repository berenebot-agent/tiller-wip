*** DRAFT FOR LEGAL REVIEW ***

This document is an engineering first draft prepared for legal review. It has
not been reviewed or approved by a qualified lawyer. It contains unresolved
[PLACEHOLDER] tokens. Do not publish it to end users or rely on it as legal
advice. Do not treat any part of it as final until a lawyer has signed off.

================================================================
ACCEPTABLE USE POLICY
================================================================

Operator:    [OPERATOR_LEGAL_NAME]
Effective:   [EFFECTIVE_DATE]

This Acceptable Use Policy ("AUP") forms part of, and is incorporated by
reference into, the Terms of Service. It applies to the hosted service operated
under the name "tiller-router" ("Hosted Tiller", "the Service"). Terms used but
not defined here have the meaning given in the Terms of Service.

1. PURPOSE

1.1 Hosted Tiller is a routing and control-plane service for AI model providers
that you configure with your own credentials. This AUP sets out what you must
not do with the Service. It is enforced to protect the Service, our users,
Providers and third parties.

1.2 This AUP is not exhaustive. Conduct that is unlawful, harmful to the
Service or others, or contrary to the spirit of the Terms of Service is
prohibited even if it is not listed.

2. PROHIBITED USES

You must not:

2.1 General-purpose proxy and tunnelling
  (a) use the Service as a general-purpose proxy, tunnel, VPN, onion-routing
      relay, port-forwarder or onward relay for traffic unrelated to your
      configured Providers;
  (b) use the Service to proxy arbitrary HTTP or non-inference traffic;
  (c) use the Service to evade network controls, geo-restrictions, sanctions,
      or access controls imposed by the Service or a third party.

2.2 Private and internal targeting
  (a) attempt to make the Service connect to private, loopback, link-local,
      multicast, reserved or cloud-metadata addresses, or to any internal
      service, database or control-plane endpoint;
  (b) attempt to bypass the hosted outbound network policy, including through
      redirects, DNS rebinding, hostname confusion, URL parsing tricks, or by
      configuring an endpoint that resolves to a blocked address;
  (c) probe, scan or enumerate our infrastructure, our network or any other
      user's resources.

2.3 Credential misuse
  (a) connect a provider credential you do not own or are not authorised to use;
  (b) share, resell, sublicense or make available provider credentials in
      breach of the applicable Provider's terms;
  (c) use the Service to circumvent a Provider's rate limits, quotas, billing
      or access restrictions, including by cycling credentials or accounts;
  (d) use another person's account, client key or credential without authority;
  (e) attempt to obtain, decrypt, infer or access provider credentials or
      secrets belonging to another user or to us.

2.4 Breaking the law or the rights of others
  (a) use the Service for unlawful purposes or to facilitate unlawful activity;
  (b) generate, distribute or process material that infringes intellectual
      property rights, that is defamatory, or that is unlawful in the relevant
      jurisdiction;
  (c) process content you are not lawfully permitted to process, or that you do
      not have the necessary rights or consents to send to a Provider;
  (d) use the Service to harm, harass, threaten or exploit any person,
      including to generate child sexual abuse material or other prohibited
      content.

2.5 Attacking the Service or other users
  (a) overload, disrupt, degrade or interfere with the Service, its
      infrastructure, or other users' use of the Service, including through
      denial-of-service, resource exhaustion, or abusive traffic patterns;
  (b) attempt to gain unauthorised access to another account's data, or to any
      system, network or data you are not authorised to access;
  (c) upload or transmit malware, or anything designed to interfere with the
      operation of any system;
  (d) reverse-engineer, decompile or attempt to derive the master key or other
      secrets, except to the extent permitted by the AGPL or by law.

2.6 Circumventing limits and controls
  (a) create multiple accounts, or use multiple credentials, to circumvent plan
      quotas, fair-use limits, suspensions or other controls;
  (b) falsify information provided to us, including in account registration.

2.7 Provider terms
  (a) use the Service in breach of any Provider's terms of service,
      acceptable-use policy, developer policy or other applicable rules;
  (b) connect a Provider where that Provider's terms prohibit the use of its
      service through a third-party router or proxy.

3. RESPONSIBILITY FOR CONTENT

3.1 You are responsible for the content you send through the Service and for
the outputs you choose to use or distribute.

3.2 You must not use the Service in any way that would cause us to breach any
law or the rights of any third party.

4. ENFORCEMENT AND RESPONSE TIMES

4.1 Report abuse to [SECURITY_EMAIL]. We aim to acknowledge abuse reports
within 48 hours and to take appropriate action promptly.

4.2 If we reasonably believe you have breached this AUP, we may take action
proportionate to the breach, which may include:
  (a) warning you and asking you to stop;
  (b) removing or disabling specific content, providers or client keys;
  (c) limiting quotas or features;
  (d) suspending your account; or
  (e) terminating your account.

4.3 For serious, urgent or repeated breaches - including credential misuse,
private-network targeting, proxy abuse, or unlawful content - we may act
immediately, including suspending your account without prior notice.

4.4 We may report unlawful activity to law enforcement or other appropriate
authorities where required or permitted by law.

4.5 You may contact [SECURITY_EMAIL] to appeal an enforcement decision. We will
review appeals in good faith but are not obliged to reverse a decision where we
reasonably believe the risk to the Service or others remains.

5. CHANGES

5.1 We may update this AUP from time to time. Material changes will be
communicated as described in the Terms of Service. Continued use of the Service
after the effective date constitutes acceptance of the updated AUP.

6. CONTACT

Operator:        [OPERATOR_LEGAL_NAME]
Abuse contact:   [SECURITY_EMAIL]
Address:         [REGISTERED_ADDRESS]

================================================================
END OF DRAFT ACCEPTABLE USE POLICY
================================================================
