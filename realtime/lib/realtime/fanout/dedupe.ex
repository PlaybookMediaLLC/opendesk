defmodule Realtime.Fanout.Dedupe do
  @moduledoc """
  Drops envelopes this node has already broadcast.

  The outbox relay delivers at least once: it publishes to NATS and then marks
  the row, so a failure in between publishes the same event again, seconds
  later. An envelope is recognised by its event `id` (or, without one, by its
  workspace and `seq`, the outbox row's key), remembered for ten minutes. Only
  an exact repeat is dropped: every distinct event is broadcast, whatever its
  `seq` relative to the ones before it, so an event that arrives after a later
  one still reaches the sockets (the relay publishes a workspace's events in
  `seq` order, and clients replay any gap through `GET /v1/events`).
  """

  use GenServer

  @table __MODULE__
  @ttl_ms 600_000
  @sweep_ms 30_000

  def start_link(opts), do: GenServer.start_link(__MODULE__, opts, name: __MODULE__)

  @doc "`:ok` the first time an envelope of workspace `wid` is seen, `:duplicate` after."
  @spec check(String.t(), map()) :: :ok | :duplicate
  def check(wid, %{"id" => id}) when is_binary(id) and id != "", do: remember({wid, :id, id})
  def check(wid, %{"seq" => seq}) when is_integer(seq), do: remember({wid, :seq, seq})
  def check(_wid, _envelope), do: :ok

  defp remember(key) do
    if :ets.insert_new(@table, {key, System.monotonic_time(:millisecond)}),
      do: :ok,
      else: :duplicate
  end

  @doc false
  def sweep(now \\ System.monotonic_time(:millisecond)) do
    cutoff = now - @ttl_ms
    :ets.select_delete(@table, [{{:_, :"$1"}, [{:<, :"$1", cutoff}], [true]}])
  end

  @impl true
  def init(_opts) do
    :ets.new(@table, [:named_table, :public, :set])
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
