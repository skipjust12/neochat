#!/usr/bin/env python3
"""Test official Poe transports with your key locally. Makes small paid queries.
Never prints the key. No SDK, cookies, browser login, or bot creation needed.
"""
import argparse, getpass, json, os, re, sys, urllib.request, urllib.error
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument("bots",nargs="*",default=["GPT-6-Luna","Claude-Opus-5.5"])
args=parser.parse_args()
if any(not re.fullmatch(r"[A-Za-z0-9_.-]{1,80}",bot) for bot in args.bots):parser.error("Use bot handles, not URLs")
key=(os.environ.get('POE_API_KEY') or getpass.getpass('Poe API key (hidden): ')).strip()
if not key.strip(): sys.exit('No key supplied')
for bot in args.bots:
    routes=[('/v1/chat/completions',{'model':bot,'messages':[{'role':'user','content':'Reply only OK.'}],'max_tokens':16,'stream':False}),
            ('/v1/responses',{'model':bot,'input':'Reply only OK.','max_output_tokens':16,'store':False}),
            ('/bot/'+bot,{'version':'1.2','type':'query','query':[{'role':'user','content':'Reply only OK.'}],'user_id':'','conversation_id':'','message_id':'','skip_system_prompt':True})]
    if bot.startswith('Claude-'):
        routes.insert(1,('/v1/messages',{'model':bot,'messages':[{'role':'user','content':'Reply only OK.'}],'max_tokens':16}))
    print('\nBOT',bot,flush=True)
    for path,body in routes:
        request=urllib.request.Request('https://api.poe.com'+path,data=json.dumps(body).encode(),headers={'Authorization':'Bearer '+key.strip(),'Content-Type':'application/json','anthropic-version':'2023-06-01'})
        try:
            with urllib.request.urlopen(request,timeout=90) as response:
                data=response.read(32768).decode(errors='replace')
                print(path,'HTTP',response.status,data.replace(key,'[REDACTED]')[:1800],flush=True)
        except urllib.error.HTTPError as error:
            data=error.read(4096).decode(errors='replace')
            print(path,'HTTP',error.code,data.replace(key,'[REDACTED]')[:1000],flush=True)
            if error.code in (401,402,429): break
        except Exception as error:
            print(path,type(error).__name__,str(error).replace(key,'[REDACTED]'),flush=True)
