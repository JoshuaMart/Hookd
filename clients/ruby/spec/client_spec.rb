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
        'smtp' => 'abc123@hookd.example.com',
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
        expect(hook.smtp).to eq('abc123@hookd.example.com')
      end
    end

    context 'when the server runs no mail listener' do
      before do
        stub_request(:post, "#{server}/register")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 200, body: hook_response.except('smtp').to_json,
                     headers: { 'Content-Type' => 'application/json' })
      end

      it 'leaves smtp nil' do
        expect(client.register.smtp).to be_nil
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
        },
        {
          'type' => 'smtp',
          'timestamp' => '2024-01-01T00:02:00Z',
          'source_ip' => '9.10.11.12',
          'data' => { 'mail_from' => 'x@vendor.test', 'subject' => 'hi', 'tag' => 'vendor' }
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
        expect(interactions.size).to eq(3)

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

        expect(interactions[2].type).to eq('smtp')
        expect(interactions[2].smtp?).to be true
        expect(interactions[2].dns?).to be false
        expect(interactions[2].http?).to be false
        expect(interactions[2].data['tag']).to eq('vendor')
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

  describe '#register long-lived' do
    let(:long_lived_response) do
      {
        'id' => 'abc123',
        'dns' => 'abc123.hookd.example.com',
        'http' => 'http://abc123.hookd.example.com',
        'https' => 'https://abc123.hookd.example.com',
        'created_at' => '2025-10-01T10:30:00Z',
        'expires_at' => '2025-10-08T10:30:00Z',
        'metadata' => { 'field' => 'profile.bio' }
      }
    end

    before do
      stub_request(:post, "#{server}/register")
        .with(
          headers: { 'X-API-Key' => token, 'Content-Type' => 'application/json' },
          body: { ttl: '7d', metadata: { field: 'profile.bio' } }.to_json
        )
        .to_return(status: 200, body: long_lived_response.to_json, headers: { 'Content-Type' => 'application/json' })
    end

    it 'sends ttl and metadata and exposes them on the hook' do
      hook = client.register(ttl: '7d', metadata: { field: 'profile.bio' })
      expect(hook).to be_a(Hookd::Hook)
      expect(hook.expires_at).to eq('2025-10-08T10:30:00Z')
      expect(hook.metadata).to eq({ 'field' => 'profile.bio' })
    end
  end

  describe '#activity' do
    let(:activity_response) do
      {
        'hooks' => [
          {
            'hook' => {
              'id' => 'abc123',
              'dns' => 'abc123.hookd.example.com',
              'http' => 'http://abc123.hookd.example.com',
              'https' => 'https://abc123.hookd.example.com',
              'created_at' => '2025-10-01T10:30:00Z',
              'expires_at' => '2025-10-08T10:30:00Z',
              'metadata' => { 'n' => '1' }
            },
            'pending_count' => 3,
            'last_interaction_at' => '2025-10-03T14:12:00Z',
            'last_seq' => 7
          }
        ]
      }
    end

    context 'when hooks have fired' do
      before do
        stub_request(:get, "#{server}/activity")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 200, body: activity_response.to_json, headers: { 'Content-Type' => 'application/json' })
      end

      it 'returns HookActivity objects' do
        activity = client.activity
        expect(activity.length).to eq(1)
        expect(activity.first).to be_a(Hookd::HookActivity)
        expect(activity.first.hook).to be_a(Hookd::Hook)
        expect(activity.first.hook.id).to eq('abc123')
        expect(activity.first.pending_count).to eq(3)
        expect(activity.first.last_interaction_at).to eq('2025-10-03T14:12:00Z')
        expect(activity.first.last_seq).to eq(7)
      end
    end

    context 'when nothing has fired' do
      before do
        stub_request(:get, "#{server}/activity")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 200, body: { 'hooks' => [] }.to_json, headers: { 'Content-Type' => 'application/json' })
      end

      it 'returns an empty array' do
        expect(client.activity).to eq([])
      end
    end
  end

  describe '#poll_batch' do
    let(:hook_id1) { 'abc123' }
    let(:hook_id2) { 'def456' }
    let(:hook_id3) { 'ghi789' }

    context 'when successful with multiple hooks' do
      let(:response_body) do
        {
          'results' => {
            hook_id1 => {
              'interactions' => [
                {
                  'id' => 'int_1',
                  'type' => 'dns',
                  'timestamp' => '2024-01-01T00:00:00Z',
                  'source_ip' => '1.2.3.4',
                  'data' => { 'qname' => 'test.example.com', 'qtype' => 'A' }
                },
                {
                  'id' => 'int_2',
                  'type' => 'http',
                  'timestamp' => '2024-01-01T00:01:00Z',
                  'source_ip' => '5.6.7.8',
                  'data' => { 'method' => 'GET', 'path' => '/test' }
                }
              ]
            },
            hook_id2 => {
              'interactions' => [
                {
                  'id' => 'int_3',
                  'type' => 'dns',
                  'timestamp' => '2024-01-01T00:02:00Z',
                  'source_ip' => '9.10.11.12',
                  'data' => { 'qname' => 'test2.example.com', 'qtype' => 'AAAA' }
                }
              ]
            },
            hook_id3 => {
              'interactions' => []
            }
          }
        }
      end

      before do
        stub_request(:post, "#{server}/poll")
          .with(
            headers: { 'X-API-Key' => token, 'Content-Type' => 'application/json' },
            body: [hook_id1, hook_id2, hook_id3].to_json
          )
          .to_return(status: 200, body: response_body.to_json, headers: { 'Content-Type' => 'application/json' })
      end

      it 'returns hash with results for each hook' do
        results = client.poll_batch([hook_id1, hook_id2, hook_id3])

        expect(results).to be_a(Hash)
        expect(results.keys).to contain_exactly(hook_id1, hook_id2, hook_id3)

        # Check hook_id1 (2 interactions)
        expect(results[hook_id1][:interactions]).to be_an(Array)
        expect(results[hook_id1][:interactions].length).to eq(2)
        expect(results[hook_id1][:interactions].first).to be_a(Hookd::Interaction)
        expect(results[hook_id1][:interactions].first.type).to eq('dns')
        expect(results[hook_id1][:interactions].last.type).to eq('http')

        # Check hook_id2 (1 interaction)
        expect(results[hook_id2][:interactions]).to be_an(Array)
        expect(results[hook_id2][:interactions].length).to eq(1)
        expect(results[hook_id2][:interactions].first).to be_a(Hookd::Interaction)
        expect(results[hook_id2][:interactions].first.type).to eq('dns')

        # Check hook_id3 (0 interactions)
        expect(results[hook_id3][:interactions]).to be_an(Array)
        expect(results[hook_id3][:interactions]).to be_empty
      end
    end

    context 'when hook not found' do
      let(:response_body) do
        {
          'results' => {
            'nonexistent' => {
              'error' => 'Hook not found'
            },
            hook_id1 => {
              'interactions' => []
            }
          }
        }
      end

      before do
        stub_request(:post, "#{server}/poll")
          .with(
            headers: { 'X-API-Key' => token, 'Content-Type' => 'application/json' },
            body: ['nonexistent', hook_id1].to_json
          )
          .to_return(status: 200, body: response_body.to_json, headers: { 'Content-Type' => 'application/json' })
      end

      it 'returns error for non-existent hook' do
        results = client.poll_batch(['nonexistent', hook_id1])

        expect(results['nonexistent'][:error]).to eq('Hook not found')
        expect(results['nonexistent'][:interactions]).to be_an(Array)
        expect(results['nonexistent'][:interactions]).to be_empty
        expect(results[hook_id1][:interactions]).to be_an(Array)
      end
    end

    context 'when hook_ids is empty' do
      it 'raises ArgumentError' do
        expect { client.poll_batch([]) }.to raise_error(ArgumentError, 'hook_ids cannot be empty')
      end
    end

    context 'when hook_ids is not an array' do
      it 'raises ArgumentError' do
        expect { client.poll_batch('not-an-array') }.to raise_error(ArgumentError, 'hook_ids must be an array')
      end
    end

    context 'when authentication fails' do
      before do
        stub_request(:post, "#{server}/poll")
          .with(
            headers: { 'X-API-Key' => token, 'Content-Type' => 'application/json' },
            body: [hook_id1].to_json
          )
          .to_return(status: 401, body: 'Unauthorized')
      end

      it 'raises AuthenticationError' do
        expect { client.poll_batch([hook_id1]) }.to raise_error(Hookd::AuthenticationError)
      end
    end
  end

  describe 'response size limit' do
    let(:hook_id) { 'abc123' }

    it 'defaults to a generous ceiling' do
      expect(client.max_response_bytes).to eq(Hookd::Client::DEFAULT_MAX_RESPONSE_BYTES)
    end

    context 'when the response exceeds the limit' do
      let(:client) { described_class.new(server: server, token: token, max_response_bytes: 1024) }

      before do
        oversized = { 'interactions' => ['a' * 4096] }.to_json
        stub_request(:get, "#{server}/poll/#{hook_id}")
          .to_return(status: 200, body: oversized, headers: { 'Content-Type' => 'application/json' })
      end

      it 'raises ResponseTooLargeError' do
        expect { client.poll(hook_id) }.to raise_error(Hookd::ResponseTooLargeError, /exceeds the 1024 byte limit/)
      end
    end

    context 'when the response is under the limit' do
      let(:client) { described_class.new(server: server, token: token, max_response_bytes: 1024) }

      before do
        stub_request(:get, "#{server}/poll/#{hook_id}")
          .to_return(status: 200, body: { 'interactions' => [] }.to_json,
                     headers: { 'Content-Type' => 'application/json' })
      end

      it 'returns the parsed payload' do
        expect(client.poll(hook_id)).to eq([])
      end
    end

    context 'when the limit is disabled' do
      let(:client) { described_class.new(server: server, token: token, max_response_bytes: 0) }

      before do
        stub_request(:get, "#{server}/poll/#{hook_id}")
          .to_return(status: 200, body: { 'interactions' => [] }.to_json,
                     headers: { 'Content-Type' => 'application/json' })
      end

      it 'does not enforce a ceiling' do
        expect(client.poll(hook_id)).to eq([])
      end
    end

    context 'when a failing response carries a large body' do
      let(:client) { described_class.new(server: server, token: token, max_response_bytes: 1024) }

      before do
        stub_request(:get, "#{server}/poll/#{hook_id}")
          .to_return(status: 401, body: 'x' * 65_536)
      end

      it 'raises the status error, with a bounded message' do
        expect { client.poll(hook_id) }.to raise_error(Hookd::AuthenticationError) { |e|
          expect(e.message.bytesize).to be <= Hookd::Client::ERROR_BODY_EXCERPT_BYTES + 64
        }
      end
    end
  end

  describe 'cursor reads' do
    let(:json_headers) { { 'Content-Type' => 'application/json' } }

    describe '#read' do
      before do
        stub_request(:get, "#{server}/poll/abc123?after=4")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 200, headers: json_headers, body: {
            'interactions' => [{ 'id' => 'i5', 'seq' => 5, 'type' => 'dns', 'data' => {} }],
            'dropped_through' => 2,
            'metadata' => { 'field' => 'bio' }
          }.to_json)
      end

      it 'returns the interactions past the cursor with loss reporting' do
        read = client.read('abc123', after: 4)
        expect(read).to be_a(Hookd::CursorRead)
        expect(read.interactions.map(&:seq)).to eq([5])
        expect(read.interactions.first.id).to eq('i5')
        expect(read.dropped_through).to eq(2)
        expect(read.lost?(4)).to be(false)
        expect(read.lost?(1)).to be(true)
        expect(read.metadata).to eq('field' => 'bio')
      end

      it 'rejects a negative or missing cursor' do
        expect { client.read('abc123', after: -1) }.to raise_error(ArgumentError)
        expect { client.read('abc123', after: nil) }.to raise_error(ArgumentError)
      end
    end

    describe '#ack' do
      it 'deletes through the given seq' do
        stub_request(:delete, "#{server}/poll/abc123?through=7")
          .with(headers: { 'X-API-Key' => token })
          .to_return(status: 200, headers: json_headers, body: { 'acknowledged' => 3 }.to_json)

        expect(client.ack('abc123', through: 7)).to eq(3)
      end

      it 'raises on a server error instead of reporting success' do
        stub_request(:delete, "#{server}/poll/abc123?through=7").to_return(status: 500, body: '{}')

        expect { client.ack('abc123', through: 7) }.to raise_error(Hookd::ServerError)
      end
    end

    describe '#read_batch' do
      it 'reads several hooks in one request' do
        stub_request(:post, "#{server}/read")
          .with(body: { 'after' => { 'abc123' => 1, 'missing' => 0 } }.to_json)
          .to_return(status: 200, headers: json_headers, body: {
            'results' => {
              'abc123' => { 'interactions' => [{ 'seq' => 2, 'type' => 'dns' }], 'dropped_through' => 0 },
              'missing' => { 'error' => 'Hook not found' }
            }
          }.to_json)

        results = client.read_batch('abc123' => 1, 'missing' => 0)
        expect(results['abc123'][:interactions].map(&:seq)).to eq([2])
        expect(results['abc123'][:dropped_through]).to eq(0)
        expect(results['missing'][:error]).to eq('Hook not found')
        expect(results['missing'][:interactions]).to eq([])
      end

      it 'rejects an empty batch' do
        expect { client.read_batch({}) }.to raise_error(ArgumentError)
      end
    end

    describe '#ack_batch' do
      it 'acknowledges several hooks in one request' do
        stub_request(:post, "#{server}/ack")
          .with(body: { 'through' => { 'abc123' => 2 } }.to_json)
          .to_return(status: 200, headers: json_headers, body: {
            'results' => { 'abc123' => { 'acknowledged' => 2 } }
          }.to_json)

        expect(client.ack_batch('abc123' => 2)).to eq('abc123' => { acknowledged: 2, error: nil })
      end

      it 'rejects a negative seq' do
        expect { client.ack_batch('abc123' => -1) }.to raise_error(ArgumentError)
      end
    end
  end

  describe 'batch registration and metadata filters' do
    let(:json_headers) { { 'Content-Type' => 'application/json' } }

    it 'registers one hook per spec, in order' do
      stub_request(:post, "#{server}/register")
        .with(body: { hooks: [{ ttl: '7d', metadata: { param: 'bio' } }, { metadata: { param: 'name' } }] }.to_json)
        .to_return(status: 200, headers: json_headers, body: {
          'hooks' => [{ 'id' => 'a', 'metadata' => { 'param' => 'bio' } },
                      { 'id' => 'b', 'metadata' => { 'param' => 'name' } }]
        }.to_json)

      hooks = client.register_batch([{ ttl: '7d', metadata: { param: 'bio' } }, { metadata: { param: 'name' } }])
      expect(hooks.map(&:id)).to eq(%w[a b])
      expect(hooks.last.metadata).to eq('param' => 'name')
    end

    it 'rejects an empty batch' do
      expect { client.register_batch([]) }.to raise_error(ArgumentError)
    end

    it 'lists hooks matching a metadata filter' do
      stub_request(:get, "#{server}/hooks?metadata.run_id=0f3a")
        .to_return(status: 200, headers: json_headers, body: { 'hooks' => [{ 'id' => 'a' }] }.to_json)

      expect(client.hooks(metadata: { run_id: '0f3a' }).map(&:id)).to eq(['a'])
    end

    it 'filters activity by metadata' do
      stub_request(:get, "#{server}/activity?metadata.run_id=0f3a")
        .to_return(status: 200, headers: json_headers, body: {
          'hooks' => [{ 'hook' => { 'id' => 'a' }, 'pending_count' => 1, 'last_seq' => 2 }]
        }.to_json)

      expect(client.activity(metadata: { run_id: '0f3a' }).first.last_seq).to eq(2)
    end
  end
end
