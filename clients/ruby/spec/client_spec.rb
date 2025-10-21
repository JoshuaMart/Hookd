# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Hookd::Client do
  let(:server) { 'https://hookd.example.com' }
  let(:token) { 'test-token-123' }
  let(:client) { described_class.new(server: server, token: token) }

  describe '#initialize' do
    it 'sets server and token' do
      expect(client.server).to eq(server)
      expect(client.token).to eq(token)
    end
  end

  describe '#register' do
    let(:hook_response) do
      {
        'id' => 'abc123',
        'dns' => 'abc123.hookd.example.com',
        'http' => 'http://abc123.hookd.example.com',
        'https' => 'https://abc123.hookd.example.com',
        'created_at' => '2024-01-01T00:00:00Z'
      }
    end

    context 'when registering single hook (no count parameter)' do
      before do
        stub_request(:post, "#{server}/register")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 200, body: hook_response.to_json, headers: { 'Content-Type' => 'application/json' })
      end

      it 'returns a Hook object' do
        hook = client.register
        expect(hook).to be_a(Hookd::Hook)
        expect(hook.id).to eq('abc123')
        expect(hook.dns).to eq('abc123.hookd.example.com')
        expect(hook.http).to eq('http://abc123.hookd.example.com')
        expect(hook.https).to eq('https://abc123.hookd.example.com')
      end
    end

    context 'when registering single hook (count: 1)' do
      before do
        stub_request(:post, "#{server}/register")
          .with(
            headers: { 'X-API-Key' => token, 'Content-Type' => 'application/json' },
            body: { count: 1 }.to_json
          )
          .to_return(status: 200, body: hook_response.to_json, headers: { 'Content-Type' => 'application/json' })
      end

      it 'returns a Hook object' do
        hook = client.register(count: 1)
        expect(hook).to be_a(Hookd::Hook)
        expect(hook.id).to eq('abc123')
      end
    end

    context 'when registering multiple hooks' do
      let(:multiple_hooks_response) do
        {
          'hooks' => [
            {
              'id' => 'abc123',
              'dns' => 'abc123.hookd.example.com',
              'http' => 'http://abc123.hookd.example.com',
              'https' => 'https://abc123.hookd.example.com',
              'created_at' => '2024-01-01T00:00:00Z'
            },
            {
              'id' => 'def456',
              'dns' => 'def456.hookd.example.com',
              'http' => 'http://def456.hookd.example.com',
              'https' => 'https://def456.hookd.example.com',
              'created_at' => '2024-01-01T00:00:01Z'
            },
            {
              'id' => 'ghi789',
              'dns' => 'ghi789.hookd.example.com',
              'http' => 'http://ghi789.hookd.example.com',
              'https' => 'https://ghi789.hookd.example.com',
              'created_at' => '2024-01-01T00:00:02Z'
            }
          ]
        }
      end

      before do
        stub_request(:post, "#{server}/register")
          .with(
            headers: { 'X-API-Key' => token, 'Content-Type' => 'application/json' },
            body: { count: 3 }.to_json
          )
          .to_return(status: 200, body: multiple_hooks_response.to_json,
                     headers: { 'Content-Type' => 'application/json' })
      end

      it 'returns an array of Hook objects' do
        hooks = client.register(count: 3)
        expect(hooks).to be_an(Array)
        expect(hooks.size).to eq(3)

        expect(hooks[0]).to be_a(Hookd::Hook)
        expect(hooks[0].id).to eq('abc123')

        expect(hooks[1]).to be_a(Hookd::Hook)
        expect(hooks[1].id).to eq('def456')

        expect(hooks[2]).to be_a(Hookd::Hook)
        expect(hooks[2].id).to eq('ghi789')
      end
    end

    context 'when count is invalid' do
      it 'raises ArgumentError for zero' do
        expect { client.register(count: 0) }.to raise_error(ArgumentError, /positive integer/)
      end

      it 'raises ArgumentError for negative' do
        expect { client.register(count: -1) }.to raise_error(ArgumentError, /positive integer/)
      end

      it 'raises ArgumentError for non-integer' do
        expect { client.register(count: 'five') }.to raise_error(ArgumentError, /positive integer/)
      end
    end

    context 'when authentication fails' do
      before do
        stub_request(:post, "#{server}/register")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 401, body: 'Unauthorized')
      end

      it 'raises AuthenticationError' do
        expect { client.register }.to raise_error(Hookd::AuthenticationError)
      end
    end

    context 'when server error occurs' do
      before do
        stub_request(:post, "#{server}/register")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 500, body: 'Internal Server Error')
      end

      it 'raises ServerError' do
        expect { client.register }.to raise_error(Hookd::ServerError)
      end
    end

    context 'when connection fails' do
      before do
        stub_request(:post, "#{server}/register")
          .to_raise(SocketError.new('Connection refused'))
      end

      it 'raises ConnectionError' do
        expect { client.register }.to raise_error(Hookd::ConnectionError)
      end
    end
  end

  describe '#poll' do
    let(:hook_id) { 'abc123' }
    let(:interactions_response) do
      [
        {
          'type' => 'dns',
          'timestamp' => '2024-01-01T00:00:00Z',
          'source_ip' => '1.2.3.4',
          'data' => { 'query' => 'test.abc123.hookd.example.com' }
        },
        {
          'type' => 'http',
          'timestamp' => '2024-01-01T00:01:00Z',
          'source_ip' => '5.6.7.8',
          'data' => { 'method' => 'GET', 'path' => '/' }
        }
      ]
    end

    context 'when successful with interactions' do
      before do
        stub_request(:get, "#{server}/poll/#{hook_id}")
          .with(headers: { 'X-API-Key' => token })
          .to_return(
            status: 200,
            body: { 'interactions' => interactions_response }.to_json,
            headers: { 'Content-Type' => 'application/json' }
          )
      end

      it 'returns array of Interaction objects' do
        interactions = client.poll(hook_id)
        expect(interactions).to be_an(Array)
        expect(interactions.size).to eq(2)

        expect(interactions[0]).to be_a(Hookd::Interaction)
        expect(interactions[0].type).to eq('dns')
        expect(interactions[0].source_ip).to eq('1.2.3.4')
        expect(interactions[0].dns?).to be true
        expect(interactions[0].http?).to be false

        expect(interactions[1]).to be_a(Hookd::Interaction)
        expect(interactions[1].type).to eq('http')
        expect(interactions[1].source_ip).to eq('5.6.7.8')
        expect(interactions[1].dns?).to be false
        expect(interactions[1].http?).to be true
      end
    end

    context 'when no interactions' do
      before do
        stub_request(:get, "#{server}/poll/#{hook_id}")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 200, body: '{"interactions":[]}', headers: { 'Content-Type' => 'application/json' })
      end

      it 'returns empty array' do
        interactions = client.poll(hook_id)
        expect(interactions).to eq([])
      end
    end

    context 'when hook not found' do
      before do
        stub_request(:get, "#{server}/poll/#{hook_id}")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 404, body: 'Hook not found')
      end

      it 'raises NotFoundError' do
        expect { client.poll(hook_id) }.to raise_error(Hookd::NotFoundError)
      end
    end

    context 'when authentication fails' do
      before do
        stub_request(:get, "#{server}/poll/#{hook_id}")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 401, body: 'Unauthorized')
      end

      it 'raises AuthenticationError' do
        expect { client.poll(hook_id) }.to raise_error(Hookd::AuthenticationError)
      end
    end
  end

  describe '#metrics' do
    let(:metrics_response) do
      {
        'total_hooks' => 42,
        'total_interactions' => 1337,
        'uptime_seconds' => 86_400
      }
    end

    context 'when successful' do
      before do
        stub_request(:get, "#{server}/metrics")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 200, body: metrics_response.to_json, headers: { 'Content-Type' => 'application/json' })
      end

      it 'returns metrics hash' do
        metrics = client.metrics
        expect(metrics).to be_a(Hash)
        expect(metrics['total_hooks']).to eq(42)
        expect(metrics['total_interactions']).to eq(1337)
      end
    end

    context 'when authentication fails' do
      before do
        stub_request(:get, "#{server}/metrics")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 401, body: 'Unauthorized')
      end

      it 'raises AuthenticationError' do
        expect { client.metrics }.to raise_error(Hookd::AuthenticationError)
      end
    end
  end
end
