# Changelog

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
