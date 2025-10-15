# frozen_string_literal: true

require 'spec_helper'

RSpec.describe 'Hookd Integration' do
  let(:server) { 'https://hookd.example.com' }
  let(:token) { 'integration-test-token' }
  let(:client) { Hookd::Client.new(server: server, token: token) }

  describe 'full workflow' do
    let(:hook_response) do
      {
        'id' => 'test123',
        'dns' => 'test123.hookd.example.com',
        'http' => 'http://test123.hookd.example.com',
        'https' => 'https://test123.hookd.example.com',
        'created_at' => '2024-01-01T00:00:00Z'
      }
    end

    let(:interactions_response) do
      [
        {
          'type' => 'dns',
          'timestamp' => '2024-01-01T00:00:00Z',
          'data' => { 'query' => 'test.test123.hookd.example.com', 'query_type' => 'A' }
        }
      ]
    end

    before do
      stub_request(:post, "#{server}/register")
        .with(headers: { 'Authorization' => "Bearer #{token}" })
        .to_return(status: 200, body: hook_response.to_json, headers: { 'Content-Type' => 'application/json' })

      stub_request(:get, "#{server}/poll/test123")
        .with(headers: { 'Authorization' => "Bearer #{token}" })
        .to_return(status: 200, body: { 'interactions' => interactions_response }.to_json,
                   headers: { 'Content-Type' => 'application/json' })
    end

    it 'registers a hook and polls for interactions' do
      # Register a new hook
      hook = client.register
      expect(hook.id).to eq('test123')
      expect(hook.dns).to eq('test123.hookd.example.com')

      # Poll for interactions
      interactions = client.poll(hook.id)
      expect(interactions).not_to be_empty
      expect(interactions.first.type).to eq('dns')
      expect(interactions.first.dns?).to be true
    end
  end

  describe 'error handling' do
    it 'handles network errors gracefully' do
      stub_request(:post, "#{server}/register")
        .to_timeout

      expect { client.register }.to raise_error(Hookd::ConnectionError)
    end

    it 'handles invalid JSON responses' do
      stub_request(:post, "#{server}/register")
        .to_return(status: 200, body: 'invalid json', headers: { 'Content-Type' => 'application/json' })

      expect { client.register }.to raise_error(Hookd::Error)
    end
  end
end
