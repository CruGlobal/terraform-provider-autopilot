# Changelog

## [0.5.0](https://github.com/CruGlobal/terraform-provider-autopilot/compare/v0.4.0...v0.5.0) (2026-10-26)


### Added

* **app:** add auto_review, for pull request reviews AutoPilot starts on its own ([#18](https://github.com/CruGlobal/terraform-provider-autopilot/issues/18)) ([3ef3055](https://github.com/CruGlobal/terraform-provider-autopilot/commit/3ef30550172213b962fb7766862bd5530a680df4))

## [0.4.0](https://github.com/CruGlobal/terraform-provider-autopilot/compare/v0.3.1...v0.4.0) (2026-10-26)


### Added

* **app:** add approve_up_to to accepts entries, a ceiling on what reviews may approve ([#16](https://github.com/CruGlobal/terraform-provider-autopilot/issues/16)) ([bc06576](https://github.com/CruGlobal/terraform-provider-autopilot/commit/bc06576c931474afcc1230a7eb0b947229d5c98c))
* **app:** allow worker_image = "ruby-browser", the opt-in image for Rails system tests ([#17](https://github.com/CruGlobal/terraform-provider-autopilot/issues/17)) ([f4a5d62](https://github.com/CruGlobal/terraform-provider-autopilot/commit/f4a5d6203fdbc99d3bd5d7caeec44892155ccae7))
* **app:** let reviews from GitHub decide, and add can_decide_own_pr to accepts entries ([#14](https://github.com/CruGlobal/terraform-provider-autopilot/issues/14)) ([f5e0890](https://github.com/CruGlobal/terraform-provider-autopilot/commit/f5e08908335e6af1192e4acf49d5252cb8e1f885))

## [0.3.1](https://github.com/CruGlobal/terraform-provider-autopilot/compare/v0.3.0...v0.3.1) (2026-10-22)


### Changed

* build with Go 1.26.10 ([#12](https://github.com/CruGlobal/terraform-provider-autopilot/issues/12)) ([7ca5e4f](https://github.com/CruGlobal/terraform-provider-autopilot/commit/7ca5e4fe844ae9870a4873644d53741819303c0c))

## [0.3.0](https://github.com/CruGlobal/terraform-provider-autopilot/compare/v0.2.0...v0.3.0) (2026-10-20)


### Added

* **app:** choose each app's worker image and size ([#9](https://github.com/CruGlobal/terraform-provider-autopilot/issues/9)) ([f18ed9b](https://github.com/CruGlobal/terraform-provider-autopilot/commit/f18ed9bbd96ac5e5ebcd2d7884c13150fab6454b))

## [0.2.0](https://github.com/CruGlobal/terraform-provider-autopilot/compare/v0.1.0...v0.2.0) (2026-10-16)


### ⚠ BREAKING CHANGES

* refuse a provider that mixes the block and the environment ([#5](https://github.com/CruGlobal/terraform-provider-autopilot/issues/5))

### Added

* add the credential policy to autopilot_app ([#6](https://github.com/CruGlobal/terraform-provider-autopilot/issues/6)) ([6e78c1f](https://github.com/CruGlobal/terraform-provider-autopilot/commit/6e78c1f20764f9154f22455fb0a430c464d12704))
* **app:** add can_auto_merge to accepts entries ([#8](https://github.com/CruGlobal/terraform-provider-autopilot/issues/8)) ([2562bd1](https://github.com/CruGlobal/terraform-provider-autopilot/commit/2562bd1b2adf6909f9bfaac84bc42a865bd08a89))


### Fixed

* refuse a provider that mixes the block and the environment ([#5](https://github.com/CruGlobal/terraform-provider-autopilot/issues/5)) ([5750fe9](https://github.com/CruGlobal/terraform-provider-autopilot/commit/5750fe945e7d1fadf57403d6eef0af8718ec31ca))


### Changed

* build with Go 1.26.9 and golang.org/x/net v0.60.0 ([#4](https://github.com/CruGlobal/terraform-provider-autopilot/issues/4)) ([749837d](https://github.com/CruGlobal/terraform-provider-autopilot/commit/749837dd82292cf3224677109b170cc61b16433b))

## 0.1.0 (2026-10-05)


### Added

* add the AutoPilot provider, with autopilot_sender and autopilot_app ([#1](https://github.com/CruGlobal/terraform-provider-autopilot/issues/1)) ([150d28f](https://github.com/CruGlobal/terraform-provider-autopilot/commit/150d28f3e3645900d9dc24999a7a8366f322cb85))

## Changelog

All notable changes to this project are recorded here by
[release-please](https://github.com/googleapis/release-please) from
Conventional Commit messages.
