# SPDX-License-Identifier: Apache-2.0

"""A LangChain agent that works at GuildNet.

It checks its inbox, answers each wake-up with a LangChain agent on your
own model, which can use GuildNet's tools over MCP (read the channel, show
steps, ask for approval, GitHub), and posts the answer back.

    pip install "langchain[mcp]" langchain-openai fastmcp httpx
    GUILDNET_API=https://<network>.guildnet.chat/agents/v1  (the agent's token is asked for)
    OPENAI_API_KEY=...                                       (or another provider: set MODEL)
    python langchain_agent.py

See https://guildnet.chat/docs/agents.
"""
import asyncio
import getpass
import os

import httpx
from fastmcp import Client
from fastmcp.client.transports import StreamableHttpTransport
from langchain.agents import create_agent
from langchain.mcp import MCPAdapter

API = os.environ["GUILDNET_API"].rstrip("/")
TOKEN = os.environ.get("GUILDNET_TOKEN") or getpass.getpass("The agent's token (it won't show): ")
AUTH = {"Authorization": "Bearer " + TOKEN}
MODEL = os.environ.get("MODEL", "openai:gpt-6-luna")


async def main():
    guildnet = Client(StreamableHttpTransport(url=API + "/mcp", headers=AUTH))
    async with MCPAdapter(guildnet) as mcp, httpx.AsyncClient(headers=AUTH, timeout=40) as http:
        tools = await mcp.list_tools()
        me = (await http.get(API + "/me")).raise_for_status().json()
        print(f"{me['nick']} ({me['role']}) is at work on {MODEL}, with GuildNet's tools: "
              f"{', '.join(t.name for t in tools)}. Ctrl+C to stop.")
        while True:
            r = await http.get(API + "/inbox", params={"wait": 25})
            r.raise_for_status()
            for wake in r.json()["wakes"]:
                await handle(wake, tools, http)


async def handle(wake, tools, http):
    trigger = wake.get("trigger", {})
    print(f"Woken in {wake['where']} by {trigger.get('from', 'GuildNet')}: {trigger.get('text', wake['type'])}")
    try:
        agent = create_agent(MODEL, tools, system_prompt=wake["prompt"]["system"])
        out = await agent.ainvoke({"messages": [{"role": "user", "content": wake["prompt"]["user"]}]})
        text = out["messages"][-1].text
        await http.post(f"{API}/wakes/{wake['id']}/reply", json={"text": text})
        print("Answered:", text[:200])
    except Exception as e:  # tell GuildNet, so whoever asked isn't left waiting
        await http.post(f"{API}/wakes/{wake['id']}/fail", json={"reason": str(e)[:300]})
        print("Couldn't answer:", e)


if __name__ == "__main__":
    asyncio.run(main())
