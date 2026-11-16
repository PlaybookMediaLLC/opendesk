defmodule Realtime.Revocations do
  @moduledoc """
  Recent revocations, so a token issued before one cannot open a socket after it
  (`contracts/realtime.md`, "Connect").

  Each `od.control.disconnect` (`Realtime.Fanout`) records its revocation time in ms: under
  the subject for a user-wide disconnect, under the session id for one session's. Every
  node receives every disconnect (no queue group), so each keeps its own table. A node
  that starts (or reconnects to NATS) asks the others for theirs (`export/1`, `merge/2`,
  over `od.gateway.revocations`, `Realtime.Nats.Consumer`), so it also knows the
  revocations made before it was listening.
  `RealtimeWeb.SocketAuth` refuses a token whose `sub`, or `sid`, has a revocation at or
  after the token's issue time (`iat_ms`, else `iat * 1000`).

  An entry is needed only while a token issued before it can still be valid, so entries
  are swept once they are older than the token lifetime plus a margin for clock skew
  (`@ttl_ms`), measured on this node's monotonic clock.
  """

  use GenServer

  alias Realtime.Ids

  @table __MODULE__
  # At most this many entries go into one exported table.
  @max_export 20_000
  @kinds %{"sub" => :sub, "sid" => :sid}
  # Tokens live 5 minutes (`auth.WSTTL`); one more for clock skew between nodes.
  @ttl_ms 360_000
  @sweep_ms 30_000

  def start_link(opts), do: GenServer.start_link(__MODULE__, opts, name: __MODULE__)

  @doc "How long an entry is kept, in ms."
  def ttl_ms, do: @ttl_ms

  @doc """
  Records a revocation at `at` (ms since the epoch) of `{:sub, sub}` or `{:sid, sid}`.
  A later revocation of the same key replaces an earlier one; an earlier one is ignored.
  `mono` is when it was recorded on this node's monotonic clock (ms).
  """
  @spec record({:sub | :sid, String.t()}, integer(), integer()) :: :ok
  def record(key, at, mono \\ System.monotonic_time(:millisecond)) when is_integer(at) do
    case :ets.lookup(@table, key) do
      [{^key, last, _}] when last > at -> :ok
      _ -> :ets.insert(@table, {key, at, mono})
    end

    :ok
  end

  @doc """
  True when the identity's subject, or its session, was revoked at or after the token
  was issued (`issued_ms`).
  """
  @spec revoked?(map()) :: boolean()
  def revoked?(%{sub: sub, issued_ms: issued} = identity) do
    revoked_since?({:sub, sub}, issued) or
      (is_binary(identity[:sid]) and revoked_since?({:sid, identity.sid}, issued))
  end

  defp revoked_since?(key, issued) do
    case :ets.lookup(@table, key) do
      [{^key, at, _}] -> at >= issued
      [] -> false
    end
  end

  @doc """
  The live entries as `[kind, id, at, age_ms]` lists for another node: `kind` is
  `"sub"` or `"sid"`, `at` the revocation time and `age_ms` how long ago this node
  recorded it.
  """
  @spec export(integer()) :: [list()]
  def export(now \\ System.monotonic_time(:millisecond)) do
    cutoff = now - @ttl_ms

    @table
    |> :ets.tab2list()
    |> Enum.filter(fn {_key, _at, mono} -> mono >= cutoff end)
    |> Enum.take(@max_export)
    |> Enum.map(fn {{kind, id}, at, mono} ->
      [Atom.to_string(kind), id, at, max(now - mono, 0)]
    end)
  end

  @doc """
  Records the entries another node exported, each as recorded `age_ms` before `now`
  here, so it is swept when the other node's copy is. Malformed entries are skipped.
  """
  @spec merge(term(), integer()) :: :ok
  def merge(entries, now \\ System.monotonic_time(:millisecond))

  def merge(entries, now) when is_list(entries) do
    entries
    |> Enum.take(@max_export)
    |> Enum.each(fn
      [kind, id, at, age]
      when is_map_key(@kinds, kind) and is_integer(at) and at > 0 and is_integer(age) and
             age >= 0 and age < @ttl_ms ->
        if Ids.uuid?(id), do: record({@kinds[kind], Ids.normalize(id)}, at, now - age)

      _ ->
        :ok
    end)
  end

  def merge(_entries, _now), do: :ok

  @doc "Removes the entries recorded more than `ttl_ms/0` before `now` (monotonic ms)."
  def sweep(now \\ System.monotonic_time(:millisecond)) do
    cutoff = now - @ttl_ms
    :ets.select_delete(@table, [{{:_, :_, :"$1"}, [{:<, :"$1", cutoff}], [true]}])
  end

  @impl true
  def init(_opts) do
    :ets.new(@table, [:named_table, :public, :set, read_concurrency: true])
    schedule()
    {:ok, nil}
  end

  @impl true
  def handle_info(:sweep, state) do
    sweep()
    schedule()
    {:noreply, state}
  end

  defp schedule, do: Process.send_after(self(), :sweep, @sweep_ms)
end
