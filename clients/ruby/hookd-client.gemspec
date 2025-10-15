# frozen_string_literal: true

require_relative 'lib/hookd/version'

Gem::Specification.new do |spec|
  spec.name = 'hookd-client'
  spec.version = Hookd::VERSION
  spec.authors = ['Jomar']
  spec.email = ['contact@jomar.fr']

  spec.summary = 'Ruby client for Hookd interaction server'
  spec.description = 'Ruby client library for Hookd, a DNS/HTTP interaction server for security testing and debugging'
  spec.homepage = 'https://github.com/JoshuaMart/hookd'
  spec.license = 'MIT'
  spec.required_ruby_version = '>= 3.1.0'

  spec.metadata['homepage_uri'] = spec.homepage
  spec.metadata['source_code_uri'] = 'https://github.com/jomar/hookd'
  spec.metadata['rubygems_mfa_required'] = 'true'

  spec.files = Dir.glob('{lib}/**/*') + %w[README.md]
  spec.require_paths = ['lib']

  # Runtime dependencies
  # (none - uses only Ruby stdlib)
end
