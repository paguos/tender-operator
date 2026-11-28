# Changelog

## [0.4.0](https://github.com/paguos/tender-operator/compare/v0.3.0...v0.4.0) (2026-11-28)


### Features

* grant project roles to Identities from an allow list ([#9](https://github.com/paguos/tender-operator/issues/9)) ([1a3ac69](https://github.com/paguos/tender-operator/commit/1a3ac697401d74ea541f957f19c6cb33d7a8d243))

## [0.3.0](https://github.com/paguos/tender-operator/compare/v0.2.1...v0.3.0) (2026-11-22)


### Features

* expose Prometheus metrics for resources and cloud calls ([#7](https://github.com/paguos/tender-operator/issues/7)) ([1113ef1](https://github.com/paguos/tender-operator/commit/1113ef1a46ce8e07cf18dfdd9814c5ce081b5eba))

## [0.2.1](https://github.com/paguos/tender-operator/compare/v0.2.0...v0.2.1) (2026-11-22)


### Bug Fixes

* write status with merge patches to avoid conflicts ([#5](https://github.com/paguos/tender-operator/issues/5)) ([e4fe4bd](https://github.com/paguos/tender-operator/commit/e4fe4bda0f3abaf94500d4ee5937d635ea01cde5))

## [0.2.0](https://github.com/paguos/tender-operator/compare/v0.1.0...v0.2.0) (2026-11-22)


### ⚠ BREAKING CHANGES

* Bucket.spec.credentialsSecret and the <name>-gcs Secret are gone. Create an Identity and list it under spec.access instead.

### Features

* split service accounts into an Identity kind ([#3](https://github.com/paguos/tender-operator/issues/3)) ([a863fd0](https://github.com/paguos/tender-operator/commit/a863fd08a84fad07297ee1a24df89752f37d7e2d))

## 0.1.0 (2026-11-22)


### Features

* Bucket operator for Google Cloud Storage ([9b44d8c](https://github.com/paguos/tender-operator/commit/9b44d8c19bf7b55598e279bd07997bfe236efe32))


### Miscellaneous Chores

* restart versioning at 0.1.0 ([7c3356a](https://github.com/paguos/tender-operator/commit/7c3356a33f4d650178e47d6e4149ae400bc8b6fc))
