# Changelog

## [0.8.0](https://github.com/ecoma-io/opencode-free-proxy/compare/v0.7.0...v0.8.0) (2026-09-22)


### Features

* **upstream:** origin TLS ClientHello parity with the official opencode client ([#49](https://github.com/ecoma-io/opencode-free-proxy/issues/49)) ([6d169e4](https://github.com/ecoma-io/opencode-free-proxy/commit/6d169e489909e5ac918d073d31c1bfc2ac98f3da))

## [0.7.0](https://github.com/ecoma-io/opencode-free-proxy/compare/v0.6.0...v0.7.0) (2026-09-22)


### Features

* **upstream:** upstream error evidence / forensics layer ([#46](https://github.com/ecoma-io/opencode-free-proxy/issues/46)) ([95ce95e](https://github.com/ecoma-io/opencode-free-proxy/commit/95ce95edf46c6f124e2ef0bf15362649606ae25b))

## [0.6.0](https://github.com/ecoma-io/opencode-free-proxy/compare/v0.5.0...v0.6.0) (2026-09-21)


### Features

* **config:** hot-reloadable log-level via zerolog ([#44](https://github.com/ecoma-io/opencode-free-proxy/issues/44)) ([c7bcfd1](https://github.com/ecoma-io/opencode-free-proxy/commit/c7bcfd1335bd736c229f83cbb9ac4ae516665464))


### Bug Fixes

* **cmd:** bound inbound listener with ReadHeaderTimeout and IdleTimeout ([#36](https://github.com/ecoma-io/opencode-free-proxy/issues/36)) ([44a3e93](https://github.com/ecoma-io/opencode-free-proxy/commit/44a3e93555284d94b65bb84ca2cb692abce36b4d))
* **identity:** stable terminal session fallback, client-tool-keyed session translation ([#37](https://github.com/ecoma-io/opencode-free-proxy/issues/37)) ([4436380](https://github.com/ecoma-io/opencode-free-proxy/commit/443638039590d6754e0fd68fa064945796fe230d))
* **jsonx:** full-string JS Number coercion and direct jsonx tests ([#35](https://github.com/ecoma-io/opencode-free-proxy/issues/35)) ([0c4c264](https://github.com/ecoma-io/opencode-free-proxy/commit/0c4c264de2a8bf8143a6c283e9ce45a7cf18b784))
* **relay:** js-semantics parity across relay, cloak and usage ([#42](https://github.com/ecoma-io/opencode-free-proxy/issues/42)) ([d994faa](https://github.com/ecoma-io/opencode-free-proxy/commit/d994faa72e0b20667ca9c877fcce903680058f6b))
* **router:** match js for cors wildcard, models empty list, non-sse guard ([#39](https://github.com/ecoma-io/opencode-free-proxy/issues/39)) ([e96bc03](https://github.com/ecoma-io/opencode-free-proxy/commit/e96bc03a30d7aad011d293f307768fe3568f788a))
* **routing:** weight-0 egress could head wrr route after eligibility flap ([#38](https://github.com/ecoma-io/opencode-free-proxy/issues/38)) ([01e7a12](https://github.com/ecoma-io/opencode-free-proxy/commit/01e7a1295dfbfb7b6688bda6078b67a1a6c7aae7))
* **translate:** js-semantics parity for gates, raw values and key coercion ([#41](https://github.com/ecoma-io/opencode-free-proxy/issues/41)) ([aa8dcfb](https://github.com/ecoma-io/opencode-free-proxy/commit/aa8dcfbdca9973fe9b3980b32dfe79543a446622))
* **upstream:** close egress and transport-boundary audit gaps ([#40](https://github.com/ecoma-io/opencode-free-proxy/issues/40)) ([9575e59](https://github.com/ecoma-io/opencode-free-proxy/commit/9575e59159773ab4df0d979ece4fe4a303603596))


### Documentation

* correct config mount reload guidance, stop grace, env placeholders ([#33](https://github.com/ecoma-io/opencode-free-proxy/issues/33)) ([e4eab42](https://github.com/ecoma-io/opencode-free-proxy/commit/e4eab429ac10ad20e212dc11a0e9ddba311ad819))
* correct doc references, env table, and e2e coverage notes ([#34](https://github.com/ecoma-io/opencode-free-proxy/issues/34)) ([a642ab8](https://github.com/ecoma-io/opencode-free-proxy/commit/a642ab8ea9a5f051c8dfbe7558c39afa82be3680))
* correct mount/reload guidance, stop grace, env placeholders ([e4eab42](https://github.com/ecoma-io/opencode-free-proxy/commit/e4eab429ac10ad20e212dc11a0e9ddba311ad819))

## [0.5.0](https://github.com/ecoma-io/opencode-free-proxy/compare/v0.4.0...v0.5.0) (2026-09-21)


### ⚠ BREAKING CHANGES

* **config:** PORT/OFP_CONFIG/OFP_CONFIG_POLL_MS/OFP_SHUTDOWN_GRACE are no longer read; set the OCFP_-prefixed equivalents.

### Features

* **config:** ocfp_ env prefix, remove inbound auth keys, 55s grace, dev docker builds ([#29](https://github.com/ecoma-io/opencode-free-proxy/issues/29)) ([0bf9d00](https://github.com/ecoma-io/opencode-free-proxy/commit/0bf9d0073b87bd297e3feff0fd9dfbda8f31baa0))

## [0.4.0](https://github.com/ecoma-io/opencode-free-proxy/compare/v0.3.0...v0.4.0) (2026-09-21)


### ⚠ BREAKING CHANGES

* **config:** move env settings into OFP_CONFIG runtime snapshot ([#22](https://github.com/ecoma-io/opencode-free-proxy/issues/22))

### Features

* **config:** move env settings into OFP_CONFIG runtime snapshot ([#22](https://github.com/ecoma-io/opencode-free-proxy/issues/22)) ([cf93063](https://github.com/ecoma-io/opencode-free-proxy/commit/cf93063be67a47aed719a9f2a8859967cec9664a))


### Bug Fixes

* **router:** bind one request to one Runtime generation; complete config-migration docs ([#25](https://github.com/ecoma-io/opencode-free-proxy/issues/25)) ([d4a59ec](https://github.com/ecoma-io/opencode-free-proxy/commit/d4a59ecb99911dc1693b5c8fdc1ef206a8a4dc78))

## [0.3.0](https://github.com/ecoma-io/opencode-free-proxy/compare/v0.2.0...v0.3.0) (2026-09-20)


### Features

* accept socks5h (remote-resolve) SOCKS5 egress ([#16](https://github.com/ecoma-io/opencode-free-proxy/issues/16)) ([bf3b20a](https://github.com/ecoma-io/opencode-free-proxy/commit/bf3b20ab47a7b4fe2b969d923362dc461c422a4e)), closes [#8](https://github.com/ecoma-io/opencode-free-proxy/issues/8)


### Bug Fixes

* resolve open code-scanning alerts — explicit TLS 1.2 floor, overflow-free allocation hint ([#19](https://github.com/ecoma-io/opencode-free-proxy/issues/19)) ([480c70e](https://github.com/ecoma-io/opencode-free-proxy/commit/480c70e4bbb7b50c635bfa95d61bc9f70a179eb1))

## [0.2.0](https://github.com/ecoma-io/opencode-free-proxy/compare/v0.1.0...v0.2.0) (2026-09-20)


### Features

* production-hardening — health & scheduler state lifecycle, snapshot immutability ([#10](https://github.com/ecoma-io/opencode-free-proxy/issues/10)) ([246c833](https://github.com/ecoma-io/opencode-free-proxy/commit/246c833eedcdee5dbd8a507f568b214a62d3efa6))


### Bug Fixes

* **ci:** run the gates on merge_group so the merge queue can merge ([#14](https://github.com/ecoma-io/opencode-free-proxy/issues/14)) ([dec7acf](https://github.com/ecoma-io/opencode-free-proxy/commit/dec7acf1950808549e1152ea22bfe4926e6dd050)), closes [#13](https://github.com/ecoma-io/opencode-free-proxy/issues/13)

## 0.1.0 (2026-09-20)


### Features

* complete Dockerfile, healthcheck subcommand, AGENTS.md ([66a5a96](https://github.com/ecoma-io/opencode-free-proxy/commit/66a5a962a9d816ba509174bb2e0788560409e631))
* forge the full compound opencode User-Agent like the official CLI ([db11230](https://github.com/ecoma-io/opencode-free-proxy/commit/db11230f66e20a20176ce88fd6fcdbcf19c648bc))
* multi-egress routing — config pool, fallback, health, hot reload ([#4](https://github.com/ecoma-io/opencode-free-proxy/issues/4)) ([d1be69c](https://github.com/ecoma-io/opencode-free-proxy/commit/d1be69c356363a5323ff3a8490a774c981758cee))
* OFP_EGRESS_PROXY — route upstream traffic through an HTTP proxy ([a709276](https://github.com/ecoma-io/opencode-free-proxy/commit/a709276a5ef689fa4fdb42a1a6ef34543dd5b2f0))
* opencode free provider router, ported from 9router open-sse ([60f1561](https://github.com/ecoma-io/opencode-free-proxy/commit/60f15610df5874a1c0ec57d725bbd31432527d4b))
* per-request config snapshot, 407 classification, adversarial e2e ([#5](https://github.com/ecoma-io/opencode-free-proxy/issues/5)) ([6f3421d](https://github.com/ecoma-io/opencode-free-proxy/commit/6f3421d22de613e86ad7349cf12073dbab857815))
* production-grade routing/fallback — health policy per snapshot, typed 407 boundary ([7bf259e](https://github.com/ecoma-io/opencode-free-proxy/commit/7bf259e3ec2a4f501c33db985f550341b1b28956))
* sync the full compound opencode User-Agent from GitHub ([950d950](https://github.com/ecoma-io/opencode-free-proxy/commit/950d9503050710a72f86ed940c516a73ba8bd065))


### Bug Fixes

* **release:** seed the manifest at 0.0.0 ([83e32ac](https://github.com/ecoma-io/opencode-free-proxy/commit/83e32ac1858113f3d11420226c90154e4d9beeb3))


### Documentation

* community + governance — Apache-2.0, contributing, security, templates ([4d13ceb](https://github.com/ecoma-io/opencode-free-proxy/commit/4d13ceb6b97a87b04b6773011c3c5f0f17d6795a))
* project-id recon — x-opencode-project is an unvalidated label ([5ec4d3b](https://github.com/ecoma-io/opencode-free-proxy/commit/5ec4d3b7a2b74a1ed91ac8ce14cdeca1d01cc0c1))
* recon — conversation survives mid-stream egress IP switch with rotated identity ([d8f9c86](https://github.com/ecoma-io/opencode-free-proxy/commit/d8f9c866f40741f7d4d02b4ab540787180f37791))
* recon — IP-switch verdict holds across ASN and address-family change ([2a9c057](https://github.com/ecoma-io/opencode-free-proxy/commit/2a9c057cf86a50fad260291614f7f926d1fdfe6a))
* record the toolchain gates in AGENTS and README ([94d7cc0](https://github.com/ecoma-io/opencode-free-proxy/commit/94d7cc0ac04d25266ae66736cd93624ad767bcba))
* session-continuity recon — different session on continue is safe ([bc05104](https://github.com/ecoma-io/opencode-free-proxy/commit/bc05104b00c2efd2781ffe505b322761ba365e16))
