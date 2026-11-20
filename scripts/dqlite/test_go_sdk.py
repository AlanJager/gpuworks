"""Offline source/modfile fixtures; no native compiler or global cache writes."""
import base64
import importlib.util
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import unittest
import zlib

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('prepare_go_sdk', HERE / 'prepare_go_sdk.py')
sdk = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sdk)

# Exact previous helper, frozen so migration tests do not require an old native prefix.
OLD_HELPER = zlib.decompress(base64.b64decode(
    b'eJyVWXtv2zgS/9+fgpsWkLxrK04c24kLHa5o0+5ie5ui6RYH9AKBliiHG1lSSSmJt5vvfjN86GHJ'
    b'busgtiTODMl5/GaGOjo6ei9YTgUjxS0jOU9TFpG3Gbl+/TvJ7plI6JY88OI2KwuyySIeb3m6VrRw'
    b'VyaMhDS8Zd7R0dGAb/JMFISKNciTzN7HYVok9uaWytuEr+ztXzJL7XUmB7HINiSnBZIQ8/g93FoS'
    b'4GWP9U1Z8EqwLFe5yEImZfVkW10WbJPHPGGDwX+uXv/57pL4xFnDpsqVF2ab45CmWcpDmhyvs3H0'
    b'JeEFO76fOoNPlx+uf7v6A6nvp97EO3MG11d/fnh1GVz/+vJ0NscBFkX0YjGdhQvKzmdsfr46n0fz'
    b'OIrC+fnZYkUvwml4tjiH/4hO2VkcxWw+ndHZeUTP5jG9cAbvX3589WtDItBdTE/Cs7Oz+Ty+CEH4'
    b'/ILNQMrpZDW7iFeLU3oyOZ/O5pNzFp7OpvOQRguYdxpesMnpJDQSL183ZNLzVRSezc5n03gRTeY0'
    b'PJ2HE4YscBOdhDDjxclJvGKTEzqnp4twOp2eTeaLxSparBYz6gze/Pbu8hpEfXVonh/Dv7fOnBGo'
    b'0WpsXKb8cSyz8I4VHtgwvMXhXDtXsM4CGd15+dYZDYj9IDPoPwfLiLFxNg9dAjlZeu/JW+dpMBhE'
    b'LCYRXzNZuOgcw6USwWPlKh6XgdxuEp7euUOSCZJmRTWARncNPX4E5ZKRTzQp2aUQmXAd9HMqCh7T'
    b'EBy8lAVZMUKJYOsyoYKggCVxyC9EFkLPPlTSBCtKkVp/hpVSULUi8ASjUbDaFky6w6F3yx7N2odm'
    b'K6ALiKa8LNx1NsJwkSMSPkT+H1nKzFoFk2VSgLprt/ZEmbqfkeNnZLnRPPAPFzSHxTAj1f8oSjYC'
    b'n3+sLvmGwZg/nQyt5vQEnt5FmEXskI4MHERchmilLYkpqCXSejGSZBExIeBH8Nxt66imgEVYCqML'
    b'Y/UgTuhauup7ZB8aXRTZHUsl6gKj35M5eJum1LPEWZlGMPyGJgA6+onQXISnhr3eHmy+GnMd63bo'
    b'cePqRvmRooLlgndIREC3pvZb5L4zbLhYaw70xdY8vnUlu8e2oJ3BHbH91hEsLiWCcpnepdlDSt5e'
    b'vXn38u21VeMLIlnCwLt5QdgjaC/kRbIlYAvJI4R9LklKC37PwFIhz5kzbM1q9Yu+1DSrMgFuVRMw'
    b'0D7RdoFdOPCHQ+pWDTmg1V+MDb+UWcEOKMV6B9xCwnEBOYwmNjTlMcQSLAdxwksyGkkXCcgxceyo'
    b'xpChDkSMA+uQsCK3IlqzwnV0EoOl/eQTkxvA9G0aWIXkKBCJTE5oaajDIbNShCzQoKD5WonjMLcC'
    b'zxZzM0V8By+LutxVOtjll8DWloGYJ8HDvz4NFb+C/oMgaj1vzVImaFFXEOBgacGLrTOsAjOlG4Ak'
    b'cERwSSCE6LCzfzYz33iQTjayidu8SgDa1ChErc3KaQdKd4WdlVWYjwhONlxuUHMa0pT0Qd+8zeTX'
    b'o1vUZ5v+UHrsmPaQijsbiDImFb6ohTerNyNdCbMlXG+87E2//aFjZf2EVcAHlic0ZM6SfK3tZ/we'
    b'zejcLBuGtW6pR56evseZ7NKqeTegeMA5p5Vc7BwGMUy94cJvzB8h9UGuZMXIFKsjrZu6gNCPtdtj'
    b'odkBgsb4px4YQJpKFcqXFcOh7bWgFn6+lBwSpLIfe0SHLFNVZ/Pa1OiTZk2ArP/GWzN/ZRuVaCAL'
    b'QLpKQ+Y21/2aCwfSDOBrVR7p4c9q6ObQYtv9gCqPaIK+scUyCZQsIb5Vlt1pCIyRtEOA96Fy3da0'
    b'w51wau5Es2EBR1cyS0pIFsNmbKnhLqx+/07Mulqx7+yGvPaVHwnS9iSKv3cOjD+faB81gQiPKhVg'
    b'pdysan8MF8CdwiKDKk2ZC9WpKlojrd4lzsIeuSxaSNtIuerZM3Jd0DWWFxSSfF6uoH4AD85SSOW2'
    b'a4QYpGtYgXFtvS+PfIQhE7tGFAKrdvWYpzQhNkpTBmS6EsFWLRMUll9txFPcWIJVjZz30ZK9tlQm'
    b'5H3H09r0IIOBCF8/HhLakF1vF+IFV24ctCKoKyDXUGhnBWTc3IFUtybQbSj0kflWdRvatUakxVfl'
    b'jL1sylkaXOreqzORhrw9XYFT9Vvj8aq+hHT7QEVkbsq///Yn6jo/aXZh+z7OGDmxLNOLUqUqN490'
    b'bNx8W8yP9ibGOTv9STP8ezXbzcbfqgmwEwG8bUVrf2fT6FtMbXuw7zF7+G606l9eG6rCW5rirqNS'
    b'YDDqPKcC8YBTKacOVNYNAkzqAKb3gKVN7+x26LW8unpoJXzlE3pXS+UNOsvWSHbcNMzTU18wHag9'
    b'HgTUS7r4UEVLVG5yaXuDEWQbLCr9U2WK/zW33xBvTg9awlr6dp6R9120fqFzmgAUK1PBEgWsuolB'
    b'+BPsL2ykMkCwqiyRHqyhLTpY5+VDJu4kalS3tv5zN98Wt1k61YcJjTaoV4E9Vml3ZeiGemHjsUkk'
    b'+wUPtduOx+uMHD3/+vZqCVdPR0OnI/Kff8hXW1qdkNN/HUfs/jgtkwRHIFsU5OQFeersGMpwPGcz'
    b'nad/9LxHBUcdrjKF5oP0kLas2uj5vtp+bWnqIYAk250tbU0Ez5p1qPZQ4679eLXTry3bcYqnWM2O'
    b'bNkqB/ZI3OnDljvQ9A2uxsIPhtYeMbqNgkDF9LHswKZunmwvhtWbBOOxyNWNXn+87vbW/UFqqQ5F'
    b'qa0wdFJpAmZ/RYIfHBEMl+u2kOueZ6VsyC6auR0/z8g7Ru91mdJTWhBeSJbEqAOFbkorYcJoWuZe'
    b'ZwW7qR8/TTW6qkQYkZ+b+iTj9pnpE+bRHW12j3l2rFVtvu5+Kxb2GLK8IJfqByJhR3M6JYhNIZji'
    b'B9Os0wyghWGakSoNt7eEpyR9Wt5jmx1HadDXix52c92BJm5DeWpnUa8SBMS+fa3gvRTrcgO+9V6N'
    b'uBGTIeRe3LkfBFEWQqZrcHo0igJqWMBAoUqZI8inGYcCyndtAsTKSIGPRdpefgu2QF1sc+Zjhh3Z'
    b'Li5qKHMPtw7nHiEHuWCFWXtGUBOF8sPfl+E93FJayM+nNwclq0N8KyyTHuRNLiCcVef49kqf8FuN'
    b'4Lkz9i1alPpBYdKEhElDylTSM11A3cDpw1wNZz5xFZG+DQwnhJKp1nfZeGy7ir7TfrfupDybTMyx'
    b'C3jhd70GsEVAXVbVrTntb3A6JywdqN5b4+jDMJW9/Z0j8PrlgFIQnvd/xmoGLWHSK/TO9Rm5VY+i'
    b'1t5NfN+6cr3lRhY1ANxpP1tYvO/sBstY1QDscLfyUqNY141//wnR8MeL4s6hkCmPYUJ1+JEkeACm'
    b'1xbxOIbioLGvHGrn1ruDGoP08bI+wWgdlvUaJAEgVK3URn2bd1a6JrnR73Q8DFoTOKp3bXmpblGT'
    b'LAS1e1nOUoAmRzWp+KzWgnp56sX40MWvkXny7urV78Hlf3urpG+dge1gBi4uUEB9+LSyBhcb7qjM'
    b'nuSvju/BhQIlNQiUPwYBonoQGJdsJWqN9+rWJDO3tvmIXF2bi0bbe11dqjGlOZXQljvGdvSGTJVf'
    b'v8pTxBBHqAVfQhWvG7nhDkZcbyWUDZdQ+Lonw8H/AeEQqR4='
))


# Exact pre-node-store modfile recipe, including its independently pinned helper.
PREVIOUS_HELPER = zlib.decompress(base64.b64decode(
    b'eJytGmlz2zb2u38F63aGVCrRum0ro53NJq7TaRpn7LTTHdfDgUhQZk0RLA/baur/vu/hIAGSkp3dVSc1QQIP776A'
    b'w8PDTxlNSUYtYqVRktDASrPonhTUOmfW1bufrIeouGVlYW1YEIXbKFlbxS3FURlTyyf+LXUPDw8Pok3KssIi2Rqg'
    b'5VSNQz8pYjW4JfltHK3U8I+cJeqZ5QdhxjZWSgqcYsnXn2CopsBa+lgPyiKqAOflKs2YT/O8erOtHgu6ScMopgcH'
    b'P1+8++XDmbW07DUQVa5cn22OfJKwJPJJfLRmg+DPOCro0f3EPvj17PLqx4uPOPt+4g7dqX1wdfHL5dsz7+r9m/Fs'
    b'jh9oEJDT48nMPyb0ZEbnJ6uTeTAPg8Cfn0yPV+TUn/jT4xP4F5AJnYZBSOeTGZmdBGQ6D8lpBfLz5ZkON5iOVqvJ'
    b'ajgahRN/tpqcDEMyHgLE8ZhMRzMaTsN5MBrOj6f+fOpPJuPhCNCYrkIfNhgdB/bBpzef377XIZLJ6WTkT6fT+Tw8'
    b'9WH2/JTOALvxcDU7DVfHYzIankxm8+EJ9cezydwnwTHQM/FP6XA89CXEs3caTHKyCvzp7GQ2CY+D4Zz447k/pLgE'
    b'BsHIhx1PgYIVHY7InIyPAdHJFJA+XgXHq+MZsQ8uPrzz3p99+HR2qYGdT4erk1E4PRmfhuPZCiieAOXhcEZhu+Fs'
    b'NhmHhNDJaHjqB7Op79PVFGb65GQ0mgAfgPg3l59//OHN289XAO6LXYl1UCbR4yBn/h0tXFA0/9buW3Yq9N9bMy8P'
    b'7tx0iy/XafnAsrt8kJAiuqcuqLvdP7D0X2tOXm5wKU3u3fzWfuLEmZiQND2Cf+6a8T2+Gi8TAYEEG4ASp6De2YDd'
    b'0ywmWxftysDk4CCgoRVEa5oXDlpYb8FBRSG3NzfKvXy7iaPkzulZLLMSVlQf0HIcOR9/GYlyav1K4pKeZRnLHBud'
    b'BMmKKCQ+eIkyL6wVepOMrsuYZBYCWFi29b2VF5nYvcehZbQos0Q5BcCUgALwCW5GSeCttgXNnV7PvaWPEveeJAVh'
    b'ergQJmSMFTU9OOqkR30Ioux5cmAS9QuWbSt6JDU1IXxbRUhexgVK+ImPQ9gSybCixMrBA9GAz3azdcxWjv3K7mkI'
    b'7JOBYwrBknArMjQwu2nJWZn54KlZUpAoyUEy4O8GSjo0KbJtp3gayDX1oCb8Wi0EscXcFLyCSf7cAFd0vdMFL1ZL'
    b'iRYZpZ6ciVvlcqtuJUENd4Nyk+Zicp/z2buj23z5OSspjNF4CIgwXzp2H61hAWx3aeKzgHYrFRgaxLm0LJw162Mg'
    b'A7D+Q7D8yBIAGMZknfNnidm31s8iBpIksArGYv8W+AvU5j4aolSdhMKzFVIwapi4LW4hgPYteJlYD7fwP4ilEhpE'
    b'IDBipBRFRAv0LRCL2eMW5U7Kgm2Aub62VU5j0NKIJSjcMFqXGQ1cDg0snzPeLxyWA9H3UcaSvnV+8eny4rd/L20W'
    b'hjYOr375+d2/6uHni4sPb9+/+fHj0o4Z4NP2OOcXHxUM0CLKV/369mppv1ogkJ4yQ84tK8q5GiPTasUBbK7t84sf'
    b'Prw5v7JRP/hc05DqeO5mZeJco0BeoURuhEjgXx8BLeEfvCEpKAmV0pMKUNDH6jHaUPi2nAwr/MROrtAuVIl9HgEo'
    b'A7usEiLwtxuUeUhA9QJhOxJeXgQ0y+BPFqVOr+2u1e97y34txQfCRgcOol2BLnG2x1vwHHmB+qBrFiZiwG67w4hw'
    b'Y6Cw2lh6SWCst0F95yzugzbRMHrsW3cJe0ikXssws/yBxDkMY7om/laMpKJDdndJN6DTFksANdDnKIwAM4ChaTvf'
    b'4jXg8wcSBS6QRuvEwm9ZFNCc54gIDVYhThjoB/AXzXdZeR+On3XEwxoEvaOuMNyrwLBYgJFxbw+YXUFSwLqjKSrd'
    b'9Y303mUSSBQ5G/hbEEGypuY79PMFuwMrRmPE7NTNUwjogt2mj6/mORW+6JQGxkDyQ3xQA+72+XIQL8TZHPXAaemW'
    b'o/FBB7zUIS8N0Eu7GUESlm1IHP3FCeV7Xo8WNxX+OgIABlCjwAvxzYADC3j8cnSAy0r2aD1cB/GlrRPaaTIdUFD0'
    b'CMVxajiKk/yDVOudMPf9nDZuHKawjV6vyTX8tb0GKGGZY7lUJgKe9HmWxPOoorvJOom7yAI6KG/vXmsnejzjM0qo'
    b'vUBX8tYS/KFNuJCr0iRwuHxrLNsQd0yu3Tm3jT9YlDg4tYdEKpS5BtUhwCS/QlNzz0byoL1x5HbgW+G/yseLDWzQ'
    b'ru8lHn+WrKCO1EXlK8FNeBBMHFDwNS3M5MOxv7U+ddXGrxHXnGbgGsuEJz8wRYQ+1BfpCxnE+EzzhL8ntsE921Nu'
    b'DhN9TyQa3zkpJAssmYjUTEMcPZzAUvNw7WKhZ6oVckVgNhhI/7gbcK/H5w8Ga2Ydfvfl/GIBT0+HPbsF8u+/rS+K'
    b'TSNr/I+jgN4fJWUc4xf6GBXW6LX11KKYPvLKXFrE8vC7DhYctlaVSQ5Ed0z9HT25kCOPTlsHPvXrIKqnbXVkXWrP'
    b'oGuwhE+A2B6FkBfCd55mxowEuYMQkd/qq4wfolLBXMPpVcmFU00CXjq2aJWAAn6ztGQHArYz5wDaeYQAcZLsPBjE'
    b't1aItN4TCbFYZ7Qn9q/mVaaxWG8YvGAtDdqrq+bA3vpKucM1TWjGLUbmVWAcSREVW+kQoZosSYz5oVbsgRT0r27K'
    b'UqcpEyUGCQCwM/HnFYO9F8cWblV9i3hYmyjf8DK92gsU0xH79dBRG6W/kQfgRIUO59wXpR8QmZUWwKMUr0oHTGlj'
    b'd0AXYEdjwDCchsT69RsFX/Dk6bl6UskOw6/kjIxmFY+1cAazPFGJKO9aGZyL3iop0H7SmPjUzGf+v/62wZ4auKSA'
    b'8+JrQQPJANmolJWLkG0XwzegpCU7/gcey0JuA5xrbC1077qjaXSDW7f6bC2FwRJTwtC6VDdt2963cl9H66bpZJ5t'
    b'XuxhRMNVSC4oFbw21ZvvjMGtdvcgJY3KZzFBDIztK0AylAZRGILt6rovp3YGkd31SDueSNoUPO4xLoXR2AvrS02z'
    b'7jFuFjuZ8fRiI1coaiaeppDQmkyXTRRs1Ds7kOm1o9PXclw1sUTSqGEgcw+1c78uA0Qw576V45Fjgy4hG3p9vAD+'
    b'3ONuvITDd305hgJNRhbQ4k3u8FQVJxilj4B6ZPeeqpCvBwAgtu77/l15+CNMuBBWtStup2P41Aq82kcRvHS1fcY4'
    b'K8MUK/cZZysDeFEEV85ZD5DKBlgWraOExBXreTNKp6dvvXpltMQXpoo87cqnNN3KBebmZk0m6K1Fc6aul9oJzF7i'
    b'daKl2Vv85ApPxdBkYgpf5VGaQLVOE4ptShvUlOk6IwGaJwDxlPKCycjm2Yqx+CXCQHw2EcDiDcGGg9ScY5WTypq7'
    b'yzd2tl1eypRnnGPbYBUuMoOXIcxRzSpRkvSlLfRFuO4DnJT1LWwMioLJtjubV0g5XyjoRjfVysS177925OE4p3K5'
    b'tcnu44fgG+DoRyn++bOMILvgKkIfMY0sE36cGtVZCDoHiRMUVv/Eody/EiGqQ5Rz/iaQMul4v4syu9dHSVaHB+Lz'
    b'Nf90s1d4RjbEu9Ykxji0xYMPnhYlBdLdPPeVEpW+eWmJGKBvq0+oPIGRz/NPBoXiFR45kFXO4rKQJx+6GesQdxjx'
    b'yymW+O/I7OvTC/+25SZfvglf37kH5gRLq9mzFOEL3t5HrMy59iOL9fK22QSwebDCtA8K77zIBdtwbJwt8X6II5q/'
    b'vC7mOwnTgR3kg7ALajbW4CW6I0ehpTptfLDfnQnbZ6yoYzE/RQMrBgowGPYWitf6SyNWYhATR7FIMz9wlSGY98bV'
    b'Qb/7meLJP8m279Q5nvQmS9sVvIKiAA/5luJ1zyI5X80X1UIFQ1tjB0+qdjWjZ8zgrQQ1FaSRQPZmN2a4mzt+Yle/'
    b'5vcXXJ+lW1RsqdF9BfDIqtoG1RL9WNFpz9OOF83sih/N+bcwzxmy4+FQP9XjSAmVGLL5cFjv1rWDBqSTEn5GKP1z'
    b'tZyPXS5MLWvbcdBjV8ffg8GqfgTSH0gWyEH511/LIX9OR8/VvvizB7gS1a1NE0KJ5Fdh4zfPQ/zaAyf8dR06oUEp'
    b'j9bA7Gh/ovdc/owHQRBmDOfTfV6lHRvJ1umO0yyDlA4fzjMJzS8/h6LpfVU7OCgzNCORBPBcxt6tatwqPb6d52Ht'
    b'BPHiHsKFbkYdndGd8KTjqb1MDWb3SdQzgMStkN2AuBOrAJln0Ne2dHY0iArzHEcedT2D4C5Vtgey77I0044KbqvB'
    b'rOzlRmVe4oyry11U/Y+HDKoNUcw22uv1Oq3bWnXCFhIjrSO2UMlQqxfWqBq6KTZrhoWhp81m2sKI8DvgNTpqi4Z5'
    b'Asw61sHXerADnsqBF93c/wrh7ojACxnYdyxSiBp2zSXae+oMCY02uCZq7W5GneRHCRYky3Hzogb3Ob/rNq4yHL5V'
    b'V5KjuyGSbB29AcGjDPdEO8v7WhSqxH+2/YMKn0fiOpBIf0UO8yKf1UjJzL3wS0YRQ0dLIFRGpUEp9LSErxRphVqs'
    b'2uL4o48+Hmif8T+AjLmQNyu69tqBYiX49oKuzSsOdhV7ssLbkChR2/DLohlYv7o46r7J1iV2OD/xL05Acx/iDxKy'
    b'9LyA+eDptZUuCQKPyCWOTfh9GHCV/i2LIK1YOioA8C43+iyVjHauV4djMBtr9SVqVV8VcIFQ2X2rhbp2ANm7ChXM'
    b'3BHYRCAEL3dFONk/z6/HN3sh85uGClh9FUjk6ucXIpHeHScs65bG6dLee0PlnL22AvaQ8B6nhSe3QZQTmKsiJF7c'
    b'wRJHoMj/IJK5TIdl6cNVIHfFyK1rPz5HuoGl5fBJYujJlZgXizy+uUycJiO87ut1VdHlVh5W9MhAu19091ElWLXp'
    b'19U+Qb3nDlO2A6RW9OqKxcBAFCbAX3ByLktpAups89IE39VY8CvVbogvHfxfX775cPH2J+/sN90RgEGL+qW7vMRf'
    b'7aPNElMt3lVnqu9GsVm9fEHFiT+/zFCNYdc68eECFtkPaCtqaHVZzPT9OE/YO69ShXG3nB2n6hvtKsdL7m7o/Sy5'
    b'TvXjpDsJ2tc2/otgVDfMtXThZkdg6kZ1V3ASXTe8kKUOM3n/RqoiENYgAEhOxM0lcWVMiqa+NIZvqxZbjXh3unHT'
    b'uH4gAkGtwriNAiz2rNSQD+8gYajUtt9QUl0LpHJ+o+aaLJN+QrRYWnpsTH0pSqodWatzUw/qTQ3b0F7r1tGWcQWi'
    b'22nVZLzMae1Xn+qQB2lULHqhC2uxr5tj0qHU9Nerq66TNr+r4WRA510n480LWk8Kx1xtZip5W/qITH07UtT1Uvnl'
    b'PbDahQq7M476Ov1ZDNogyjj+f3lPXxQ7N+LKsatXV51l0nP98UbKgHHG43na/hMgrXre1/+ocNRCrcS86sV3Mkr4'
    b'F604+AJFgHZMCaMb7iLhQfT6Ou9FaFckn7kT0SrSek89vCUE5uVxhngeVzbPw3zU82RgMDJtkanyocyqndp4+tbF'
    b'lXzQ2lhX1aP89hPd8iceySk+1fAFU2whFdmtqS/j88nAXV7x59tcNmR6jXzkaptDvDh7jApn1Dv4D93gnKU='
))

NODE_STORE_HELPER = zlib.decompress(base64.b64decode(
    b'eJzVG2lv20b2u38F6xYglcq0JOoOtNhs4qZF0ziw02AXriEMyaHMmiJVHo7V1P9935uDnOEhK9j9sKsgFo+ZN+++ZnR6evohpTuSUoMYuzCOqW/s0vCB5NR4mxjXb342Pof5XVLkxjbxw2Afxhsjv6N4V0TU8Ih3R+3T09OTcLtL0twg6QagZVTeB16cR/LmjmR3UejK29+zJJbXSXYSpMnW2JEchxji8Qe4lUNgLn2sboo8LAFnhbtLE49mWflkX17mdLsLwoienPxy+ebXdxfGyjA3QFTh2l6yPfdInMShR6LzTXLm/xGFOT1/cMyTTxdX1z9dvsfRD449sMfmyfXlr1evL9bXP74aTab4gvo+WcyciTcjdD6h07k7n/rTwPe96Xw8c8nCc7zxbA7/feLQceAHdOpMyGTuk/E0IIsS5MerCxWuPx66ruMOhsPA8SauMx8EZDQAiKMRGQ8nNBgHU384mM7G3nTsOc5oMAQ0xm7gwQLDmW+efHj18fWPKkTiLJyhNx6Pp9Ng4cHo6YJOALvRwJ0sAnc2IsPB3JlMB3PqjSbO1CP+DOhxvAUdjAaegHjxRoFJ5q7vjSfziRPM/MGUeKOpN6A4BW78oQcrLoAClw6GZEpGM0DUGQPSM9efubMJMU/eX74Bsj9eXsHfOm+nYzJbzAbzkRP4Cxju+9QdOZTMg2A29IYTbzgOPOICo9xgQfwRdeeEBgtn6owWg/mUatDr7KABDRwyChyXUGc6XwTOeA64ekAp8f3pkIIsB0DZyBkF3iyYDECyDvEHg8Dxp8DnJnCNM45DhhMyn/vD6WQSkMAbu958MptRMiZTwHvgjqZz4kyB7+54OvAn88V8MiQzoHQe0NEUuH118emny1+v1z9evPtwcaXAnsyHgO4Evmajkb8YTILplDoz4NZksXBdH5TRc0buyJ/PZqB3wC7iwFBnPnRBsO7EPLl896YJdjoeuPNhMJ6PFgEMAz1zQN+CwYSCkAeTCTCCAK+Gg4XnT8aeR90xjPTIfDh0QPtA5V5dffzph1evP14DuC9maUxnRRw+nmWJd09zG8zbuzP7hrnjXme9SdaZf2/v9vhwsys+J+l9dhaTPHygNjgZs39iqJ/GmKzY4lQaP9jZnflUMU5FJ6O5Vd73NEyr6780rOPEp2dZnqRUIP3E+KYTSXa7c/hvbxKG/leTrNPG6UvOwCvtwF+lZ8kDTSOyt9FRakSenPg0MPxwQ7PcQpfZWzJQYcAcqB1m62y/jcL43uoZSWrESV6+QFdoifH4SUmYUeMTiQp6kaZJapno9UmahwHxwO0XWW64GB5SuikikhoIYGmYxvdGlqd89R6DltK8SGPp5QFTArrFBtgpJf7a3ec0s3o9+44+Ctx7ghSEucaJMCBNkryiB+9a6ZEv/DB9nhwYRD0Q5r6kR1BTEcKWlYRkRZSjhJ/YfQBLIhlGGBsZhBTqs9F2uokS1zJfmD0FgUMysHQhGAJuSYYCppuWLClSD0JvEuckjDOQDASwMykdGufpvlU8NeTqelARfiMngtgiZmXrPBH8uQWuqHqnCp7PFhLNU0rXYiQulYml2pUENdz2i+0u44P7jM/re7rPVh/TgsI9Gg8BEWYry+yjNSyB7TaNPTDUdqUCQ4PEZVfk1ibpY2YCYL3P/up9EgPAICKbjF0LzL41fuFJDYl9I0+SyLsD/gK1mYeGKFQnpnBtBBSMGgbu8zvIiPoGPIyNz3fwB5IjAQ1SCjBipBRFRHN0W5BcJY97lDsp8mQLzPWUpTIagZaGSYzCDcJNkVLfZtDA8hnjvdxKMiD6IUyTuG+8vfxwdfnPf63MJAhMvL3+9Zc3/6huP15evnv946uf3q/MKAF8mh7n7eV7CQO0iLJZn15fr8wXSwTSk2bIuGWEGVNjZFqlOIDNjfn28od3r95em6gfbKxuSFWCZqdFbN2gQF6gRG65SOB/HwGt4D88ITtQEiqkJxQgp4/lZbil8G7lDEr8+Eo21y5UiUMeASgDuywzXPC3W5R5QED1fG47Al6W+zRN4SsNd1av6a7l53vDfCnEB8JGBw6idUGXGNujPXiOLEd9UDULM2tgt9liRLgwUFguLLwkMHa9RX1nLO6DNtEgfOwb93HyORZ6LcLM6gcSZXAb0Q3x9vxOKDqk61d0CzptJDGgBvocBiFgBjAUbWdLvAR8fkeiwAXScBMb+C4NfZqxpB+hwSzECXOIM/hG812V3ofhZ5yzsAZB77wtwvdKMEnEwYi4dwBMV5DksO7pDpXu5lZ47yL2BYqMDewpiCDeUP0Z+vk8uQcrRmPEcsPOdhDQObt1H1+Os0p80SmdaTeCH/yFvGFun00H8UKczVAPrIZuWQofVMArFfJKA70y6xEkTtIticI/GaFszZvh8rbEX0UAwABqFHjB32lwYAKLX5YKcFXKHq2H6SA+NFVCW02mBQqKHqFYVgVHcpK9EGrdCfPQx2rixmBy2+j16lzDT9NrgBIWGda/RczhCZ9nCDzPS7rrrBO48yyghfLm6pV2osfTXqOEmhNUJW9MwQ/ahA25Ko19i8m3wrIJsWNw5c6ZbfyehLGFQ3tIpESZaVAVAnTySzQV96wlD8oTSywHvhX+lT6eL2CCdn0v8PijSHJqCV2UvhLcxBqCiQUKvqG5nnxY5rfGh7Zmx0vENaMpuMYiZskPDOGhD/VF+MIEYnyqeMLfYlPjnrmWbg4T/TVPNL6zdpAsJLHDUzMFcfRwHEvFwzWLhZ6uVsgVjtnZmfCP3YB7PTb+7GyTGKfffXl7uYSrp9Oe2QD5F1RBkk1DY/S3c58+nMdFFOEb+hjmxvCl8dSgmD6yVouwiNXpdy0sOG3MKmIozIyWob+hJ+dyZNFpb8GrfhVE1bStiqwr5Rp0DaawARDbwwDyQnjP0swoIX5mIUTkt3wr4gevVDDXsHplcmGVg4CXlsl7X6CA36wM0VKC5fQxgHYWIkAcJFpJGvGNGTytX/OEmM/TeiKHZ7MqU5ustjyOmEv95uyyp3GwvpLucENjmjKLEXkVGEech/leOESoJgsSYX6oFHsgBfWtvUt2Vl0mUgwCAGCn488qBvMgjg3cyvoW8TC2YbZlZXq5FnYM+Ho9dNRa6a/lAThQosM490XqB0RmqQVwKcQr0wFd2tgdUAXY0hjQDKcmsX71RMLnPHl6rp6UssPwKzgjolnJYyWcwag1r0Skdy0NzkZvFedoP7uIeFTPZ/67/rbGngq4oIDx4mtBA8kAWauUpYsQbRfNN6CkBTv+Ax6LQm4LnKstzXXvpqVpdItLN1p4DYXBElPAULpUt03bPjTzUEfrtu5knm1eHGBEzVUILkgVvNHVm62Mwa1y9yAlhcpnMUEMtOVLQCKU+mEQgO2qui+GtgaR7nqkGU8EbRIe8xhX3GjMpfGloln1GLfLTmY8HW3kEkXFxHc7SGh1posmCu68WB3I9JrR6Ws5LptYPGlUMBC5h1y5X5UBPJgz38rwyLBBF5MtvZktgT8PuBor4fBZX9xDgSYiC2jxNrNYqooDtNKHQz03e7zfh73fNev9rgWv2Z5Rd2+4WobHMhFcMq2/zGqoBmCWzTab1mXqoQYiYHoF+q8y0pxj4oc0ldQjPiqnnhoJgPKSB1HVfJ5xEqWD4DMPOYlGJnJUJiGDhBqosyo2N7m4bPWdBwTGXEjH/lCbQ2zyy4tCcFnnHKhkWuemUCtMLYmpE6VlY1+D6UGotSyvE9+jfTkuYLAF2lI+GpVpm0g1m2UN05DW3a7jVIU+hEmRaTpzR6MdTWVjKQ03YUyi0mewLqoq0L7x4oW2l7PUfdvTUZqnr3PTUBDsjHbtd3YUGorTzTij9EXqVqn23PWRqsNW9poPWqPKUREPDbZHj/v/GEsiCm/FoQGOamWj+X5Ha9QUu01KfIxbAGQtvTrEEtFVdpMkOkbkiM82BFisU17TOCVrKIs10YxqSxpa+5HHMuWZrKEZySQuorQVxmDJLi6v1fvC2fR5HtsHOLukb2DHnHcSTLO1q4uUs4mcbozfjRJVef+ppUDFMWUuUsWQQ/zgfAMcvXCHX38UIaTdTEXoI9ZXRcwOjoRVeo7RSuD0vWH+HW/F+oqd5aAVjL8x1BIq3m9CMOw+SrLcVeOvb9ir24PC08oEtp1DIkzQ9rgjyOqFOEe66ydchERF0rIyeHKkLqsOKD2NVuiyVxqF/BHuxRE3S6IiF1uCqhmrEDuM+HiKBf4dJW+1refdNeL28Yuw+a1rMO/JXwsWrhlh63XPxjxsjamLdShs13GtIB4Kk0fgroSxbvwx2V8Z9c0InpfKOMSsF1VE7VvVu3smy0IxANLHMMszLna81zaNWWpo8V0d1vBiK3HThxXEBbdrqnfM4SG6U6sMj6KFzm4Ou2Puu5Ikr5Jstj0OXggoQBH1lpL/6kMt+UQp8uMbSDM7pCFya7bpJY9k2R8pntEi6f6N3KAX3nBl2pxXUO3j7v2KP+4ZJGOz2aRKsOAoNtiaF3pVjuhpI1geL4eCNGIoy8zaCHt7z7biq8fspJntJbs9GqawyL4EeG6U/cByinpewGqOU84N6GkW23P37mCcNUhmg4G6Xc+Q4ioxSKaDQbVa2woKkFZK2Oa/iC/ldHZvM2Eq5VjHDq5Znms5O3OrSyD9M0l9cVP8+edqwK53w+eaWvgxz3AmqluTJoQSirfc5G+fh/i1O8n4adtNRoOSHrmG2fnhyum5RBp3eCFMas6zfSNa2Q8WeyId29TdEq/8pSL26uH/hewVn/8/ogBfVwoeqRCNiPRfU4owaEtMWHqsJBvPoamnFHLzzy9S9K08s2UJutmtjbUUANBOogfIgVTf2lIwdsIT0agKPRWY7nMHzwDixwu7AbHIVgLSTxzdmCICUj/M9V17cbDhGQS71Ns8E132lZ5Ll3Ab24nSkG5lOcFPNLTFkLLb/TmFHIy3LmubqdU8ZW+t3PdYCoyU/Y+lzPAbOx+1UrudYr0QXmp6Wt86WWppawe8Wi9kWfPZALNKgOBtddMBr6t5s+xKSY+Eo6LY6VA6YMlic9muEV+hcB2p4lJkoB2TJPM0X8O0rPfUmrvUNmIV9VNOB1bVdBhj5b8a1Y8KMj/4m+p3ZCrOlmrLxlXXSOK9pbbAWUhk3rGzwVyph2wyP9s+QyPMQn4gldeZPNk+yo/Wagd9LXyTUlZJKZmuTP0VKLmaP7OZPP+Vk+XGLH7oo4dHqi7YFyCjT2Rt6ra1OlAsBd+c0LZ4ycG2ropopWxJGMtl2O9PUmNV/hbFfpVuCtxj+8DeWD7NPIiJSMhqvfYTD6KPMtMmvr8mYoplEnYiE9y3d5eEkAOtyi4m22dFPyqrptb58ngGjMam2Aq1qi87JT5X2UOzubq2ADk4CxVMXxHYRCAtWHVFXbGDm92Mbg9CZmfdJbDqMCovKt9e8oqvO3YZrDu7Mg+ekXybvDT85HPMdtkMPDvkhxmBsTJq49FRrMU5iuwLkcxE3SZqdKYCmc3v7KrJwsYIN7AyLDaI367FTCzgeMFZn8bPMyG89gPeZXfALj0s72aAdh91+l5WApXpV201gnrPHKbouwmt6FWltYYBr6CBv+Dk7GRHY1Bnk9XQ+KzCgv1Kyw7woYV/+uLJu8vXP68v/qk6AjBoXmi390HwU/lovRciJ3c1ROR7rStSPjyiNYIfr0hRjWHVKhljAuYZGWgramh5XFn3/TiO2ztrp3Djbjg7RtU3ymHCY04Pqo1jMU82voU7UTNSidDXB6Nqy1ZJYW47AlM7ql3Bibe38UiwPE7DGqVCFYGwGgFAcszPzvJDy0I01bFlfFr2sivE29ON29oBOB4IKhXGZSRgvmaphuz2HhKGUm37NSVVtUAo5zdyrM4y4Sd4L7Chx9rQY1GSff9Knet6UC2q2YbyWLWOpoxLEO1OqyLjOKd1WH3KYwZIo2TRkS6swb52jgmHUtFfzS7bo8r4ts6oBp21R7UnR/RIJY6ZXExX8qb0EZnqfD7vPwjlFyeRKxfK7U47bNLqzyLQBl5asr/il2K8ALvlP3qx1YqvtXR7biPqmF59y96/UtEf6tOUOCqhVmBebnq1Mor7F6U4+AJFgHJQBu5umYuEC96Ubj2ZpxzSf+ZUXqNw7D318JwqmNeaMWS9Zsq2XmM+ul6LwKBl2jxT5bvjPKu2KuPpG5fX4kLpuV2Xl+Ldz3TPrlgkp3hVwedMMblURFux+jkYGwzcZV2IbJ+JJlGvlo9c7zOIFxePYW4Neyf/BpRdFuc='
))
class GoSDKTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        env = dict(os.environ, GOFLAGS='', GOENV='off', GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local')
        cache = Path(subprocess.check_output(['go', 'env', 'GOMODCACHE'], cwd=tempfile.gettempdir(), env=env, text=True).strip())
        cls.original = cache / (sdk.MODULE + '@' + sdk.VERSION)
        cls.source_files = sdk.file_hashes(cls.original)
        if sdk.tree_digest(cls.source_files) != sdk.SOURCE_TREE_SHA256:
            raise ValueError('offline fixture requires the complete pinned SDK source')
        # Use the already-present exact toolchain directly; never fetch a toolchain.
        cls.real_go = cache / 'golang.org/toolchain@v0.0.1-go1.26.7.linux-amd64/bin/go'
        if not cls.real_go.is_file():
            raise ValueError('offline compiler fixture requires installed Go 1.26.7')

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.prefix = self.root / 'native'
        self.prefix.mkdir()
        (self.prefix / '.gpuworks-dqlite').write_text('verified-C-fingerprint\n')
        self.module = self.root / 'module-cache' / ('go-dqlite@' + sdk.VERSION)
        shutil.copytree(self.original, self.module)
        for path in self.module.rglob('*'):
            path.chmod(0o700 if path.is_dir() else 0o600)
        self.repo = self.root / 'repo'
        self.repo.mkdir()
        (self.repo / 'go.mod').write_text('module fixture/root\n\ngo 1.26.7\n\nrequire ' + sdk.MODULE + ' ' + sdk.VERSION + '\n')
        (self.repo / 'go.sum').write_text('')
        self.info = {'Path': sdk.MODULE, 'Version': sdk.VERSION, 'Dir': str(self.module)}
        self.metadata = self.root / 'module.json'
        self.metadata.write_text(json.dumps(self.info))
        self.go = self.root / 'go'
        self.go.write_text('''#!/usr/bin/env python3
import os, subprocess, sys
from pathlib import Path
if any(os.environ.get(name) != value for name, value in
       {'GOPROXY': 'off', 'GOSUMDB': 'off', 'GOTOOLCHAIN': 'local',
        'GONOPROXY': 'none', 'GOVCS': '*:off'}.items()):
    sys.exit('SDK subprocess did not enforce offline discovery')
if sys.argv[1:] == ['env', 'GOFLAGS']:
    print(os.environ.get('GOFLAGS') or os.environ.get('FIXTURE_PERSISTED_GOFLAGS', ''))
elif sys.argv[1:] == ['list', '-m', '-json', 'github.com/canonical/go-dqlite/v3']:
    if '-modfile' in os.environ.get('GOFLAGS', '') or '-overlay' in os.environ.get('GOFLAGS', ''):
        sys.exit('original SDK discovery retained a compiler override')
    print(Path(os.environ['FIXTURE_MODULE_INFO']).read_text())
elif sys.argv[1:3] == ['mod', 'edit']:
    sys.exit(subprocess.call([os.environ['FIXTURE_REAL_GO'], *sys.argv[1:]]))
else:
    sys.exit('unsupported fixture Go command')
''')
        self.go.chmod(0o700)
        self.env = dict(os.environ, GO=str(self.go), GOFLAGS='', GOENV='off', GOTOOLCHAIN='local',
                        GOPROXY='off', GOSUMDB='off', FIXTURE_REAL_GO=str(self.real_go),
                        FIXTURE_MODULE_INFO=str(self.metadata), PYTHONDONTWRITEBYTECODE='1')
        self.env.pop('FIXTURE_PERSISTED_GOFLAGS', None)

    def tearDown(self):
        self.assertEqual(sdk.file_hashes(self.original), self.source_files)
        self.temp.cleanup()
    def prepare(self, target=None):
        return sdk.prepare(self.prefix, target or self.prefix, self.info,
                           HERE / 'go-dqlite-unix-socket.patch', self.repo, str(self.real_go))

    def command(self, action, env=None):
        return subprocess.run(['python3', str(HERE / 'prepare_go_sdk.py'), action,
                               '--prefix', str(self.prefix), '--repo', str(self.repo)],
                              env=env or self.env, text=True, capture_output=True)

    def previous_private_sdk(self):
        helper = self.root / 'previous-modfile-helper.py'
        helper.write_bytes(PREVIOUS_HELPER)
        self.assertEqual(sdk.digest(helper), sdk.PREVIOUS_HELPER_SHA256)
        spec = importlib.util.spec_from_file_location('previous_modfile_sdk', helper)
        previous = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(previous)
        return previous.prepare(self.prefix, self.prefix, self.info,
                                HERE / 'go-dqlite-unix-socket.patch', self.repo, str(self.real_go))

    def test_rpc_cancel_manifest_and_original_tree_restoration(self):
        manifest = self.prepare()
        self.assertEqual(manifest['rpc_cancel_patch_sha256'], sdk.RPC_CANCEL_PATCH_SHA256)
        self.assertEqual(manifest['rpc_cancel_module_sha256'], sdk.RPC_CANCEL_MODULE_SHA256)
        self.assertEqual(sdk.tree_digest(sdk.file_hashes(self.prefix / 'go-sdk/module')),
                         sdk.RPC_CANCEL_MODULE_SHA256)
        self.assertEqual(manifest['source_files'], self.source_files)
        self.assertEqual(sdk.verify(self.prefix / 'go-sdk'), (manifest, 'modfile'))
        self.assertEqual(manifest, self.prepare())
        for name in sdk.RPC_CANCEL_PATCHED_FILES:
            self.assertEqual(sdk.digest(self.prefix / 'go-sdk/module' / name),
                             sdk.RPC_CANCEL_PATCHED_FILES[name])

    def test_rpc_cancel_rehashed_tampering_is_rejected(self):
        for name in ('go-dqlite-rpc-cancel.patch', 'prepare_go_sdk.py',
                     *('module/' + name for name in sdk.RPC_CANCEL_PATCHED_FILES)):
            with self.subTest(name=name):
                root = self.prefix / 'go-sdk'
                if root.exists():
                    shutil.rmtree(root)
                self.prepare()
                path = root / name
                path.write_bytes(path.read_bytes() + b'\n# unapproved change\n')
                manifest_path = root / 'manifest.json'
                manifest = json.loads(manifest_path.read_text())
                manifest['files'][name] = sdk.digest(path)
                manifest_path.write_text(json.dumps(manifest))
                with self.assertRaisesRegex(ValueError, 'unknown RPC cancellation SDK'):
                    sdk.verify(root)

    def test_build_fingerprint_includes_rpc_production_regression_and_checker(self):
        script = (HERE.parent / 'build_dqlite.sh').read_text()
        fingerprint = script.split('sha256sum "$script_dir/build_dqlite.sh"', 1)[1].split('for tool in', 1)[0]
        for name in ('go-dqlite-rpc-cancel.patch', 'go-dqlite-rpc-cancel-regression.patch',
                     'check_rpc_cancel.py'):
            self.assertIn('"$assets/' + name + '"', fingerprint)

    def test_node_store_prefix_remains_readable_and_unchanged(self):
        helper = self.root / 'node-store-helper.py'
        helper.write_bytes(NODE_STORE_HELPER)
        self.assertEqual(sdk.digest(helper), sdk.NODE_STORE_HELPER_SHA256)
        shutil.copyfile(HERE / 'go-dqlite-node-store.patch', helper.with_name('go-dqlite-node-store.patch'))
        spec = importlib.util.spec_from_file_location('node_store_sdk', helper)
        previous = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(previous)
        manifest = previous.prepare(self.prefix, self.prefix, self.info,
                                    HERE / 'go-dqlite-unix-socket.patch', self.repo, str(self.real_go))
        (self.prefix / '.go-sdk.lock').touch()
        before = sdk.file_hashes(self.prefix)
        self.assertEqual(sdk.verify(self.prefix / 'go-sdk'), (manifest, 'modfile'))
        own = '-modfile=' + str(self.prefix / 'go-sdk/gpuworks-native.mod')
        result = self.command('flags', dict(self.env, GOFLAGS='-trimpath ' + own))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), '-trimpath ' + own)
        self.assertEqual(sdk.file_hashes(self.prefix), before)
        target = self.prefix / 'go-sdk/prepare_go_sdk.py'
        target.write_bytes(target.read_bytes() + b'\n# unknown helper\n')
        path = self.prefix / 'go-sdk/manifest.json'
        changed = json.loads(path.read_text())
        changed['files']['prepare_go_sdk.py'] = sdk.digest(target)
        path.write_text(json.dumps(changed))
        with self.assertRaisesRegex(ValueError, 'unknown node store SDK helper'):
            sdk.verify(self.prefix / 'go-sdk')

    def test_previous_private_sdk_upgrade_adds_only_pinned_node_store_patch(self):
        previous = self.previous_private_sdk()
        self.assertEqual(sdk.verify(self.prefix / 'go-sdk'), (previous, 'modfile'))
        current = self.prepare()
        self.assertEqual(sdk.verify(self.prefix / 'go-sdk'), (current, 'modfile'))
        self.assertEqual(current['node_store_patch_sha256'], sdk.NODE_STORE_PATCH_SHA256)
        self.assertEqual(sdk.digest(self.prefix / 'go-sdk/module/client/store.go'), sdk.NODE_STORE_PATCHED_SHA256)
        self.assertEqual(sdk.file_hashes(self.module), self.source_files)
        self.assertEqual(current, self.prepare())
    def test_previous_private_sdk_unknown_helper_rejected_even_with_rehashed_manifest(self):
        self.previous_private_sdk()
        root = self.prefix / 'go-sdk'
        helper = root / 'prepare_go_sdk.py'
        helper.write_bytes(helper.read_bytes() + b'\n# unapproved override\n')
        manifest_path = root / 'manifest.json'
        manifest = json.loads(manifest_path.read_text())
        manifest['files']['prepare_go_sdk.py'] = sdk.digest(helper)
        manifest_path.write_text(json.dumps(manifest))
        with self.assertRaisesRegex(ValueError, 'unknown previous private SDK helper'):
            self.prepare()

    def test_node_store_patch_and_result_rejected_even_with_rehashed_manifest(self):
        for name in ('go-dqlite-node-store.patch', 'module/client/store.go'):
            with self.subTest(name=name):
                if (self.prefix / 'go-sdk').exists():
                    shutil.rmtree(self.prefix / 'go-sdk')
                self.prepare()
                root = self.prefix / 'go-sdk'
                target = root / name
                target.write_bytes(target.read_bytes() + b'\n')
                manifest_path = root / 'manifest.json'
                manifest = json.loads(manifest_path.read_text())
                manifest['files'][name] = sdk.digest(target)
                manifest_path.write_text(json.dumps(manifest))
                with self.assertRaisesRegex(ValueError, 'unknown node store SDK identity'):
                    sdk.verify(root)

    def test_node_store_regression_rejects_symlink_work_ancestor_before_copy(self):
        link = self.root / 'source-alias'
        link.symlink_to(self.module, target_is_directory=True)
        before = sdk.file_hashes(self.module)
        result = subprocess.run(['python3', str(HERE / 'check_node_store.py'),
                                 '--source', str(self.module), '--work-dir', str(link / 'nested')],
                                env=self.env, text=True, capture_output=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('must not traverse a symlink', result.stderr)
        self.assertEqual(sdk.file_hashes(self.module), before)
        self.assertFalse((self.module / 'nested').exists())

    def test_exact_private_copy_patch_manifest_and_own_flag_regeneration(self):
        manifest = self.prepare()
        self.assertEqual(sdk.file_hashes(self.module), self.source_files)
        self.assertEqual(sdk.digest(self.prefix / 'go-sdk/module/app/app.go'), sdk.PATCHED_SHA256)
        self.assertEqual(sdk.verify(self.prefix / 'go-sdk'), (manifest, 'modfile'))
        self.assertEqual(manifest, self.prepare())
        mod = json.loads(subprocess.check_output([str(self.real_go), 'mod', 'edit', '-json',
                       '-modfile=' + str(self.prefix / 'go-sdk/gpuworks-native.mod')], cwd=self.repo, env=self.env, text=True))
        self.assertEqual(mod['Replace'], [{'Old': {'Path': sdk.MODULE}, 'New': {'Path': str(self.prefix / 'go-sdk/module')}}])
        selected = json.loads(subprocess.check_output([str(self.real_go), 'list',
                              '-modfile=' + str(self.prefix / 'go-sdk/gpuworks-native.mod'),
                              '-m', '-json', sdk.MODULE], cwd=self.repo, env=self.env, text=True))
        self.assertEqual(selected['Version'], sdk.VERSION)
        self.assertEqual(selected['Dir'], str(self.prefix / 'go-sdk/module'))
        package = json.loads(subprocess.check_output([str(self.real_go), 'list',
                             '-modfile=' + str(self.prefix / 'go-sdk/gpuworks-native.mod'),
                             '-find', '-json', sdk.MODULE + '/app'], cwd=self.repo, env=self.env, text=True))
        self.assertEqual(package['Dir'], str(self.prefix / 'go-sdk/module/app'))
        self.assertIn('app.go', package['GoFiles'])
        self.assertIn('func WithUnixSocket(path string) Option {',
                      (self.prefix / 'go-sdk/module/app/options.go').read_text())
        own = '-modfile=' + str(self.prefix / 'go-sdk/gpuworks-native.mod')
        result = self.command('prepare', dict(self.env, GOFLAGS='-trimpath ' + own))
        self.assertEqual(result.returncode, 0, result.stderr)
        result = self.command('flags', dict(self.env, GOFLAGS='-trimpath ' + own))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), '-trimpath ' + own)
        self.assertEqual((self.repo / 'go.mod').read_text(), 'module fixture/root\n\ngo 1.26.7\n\nrequire ' + sdk.MODULE + ' ' + sdk.VERSION + '\n')
        self.assertEqual((self.repo / 'go.sum').read_text(), '')

    def test_exact_old_recipe_upgrade_replaces_only_its_proven_flag(self):
        previous = self.root / 'previous-helper.py'
        previous.write_bytes(OLD_HELPER)
        self.assertEqual(sdk.digest(previous), sdk.OLD_HELPER_SHA256)
        old_spec = importlib.util.spec_from_file_location('previous_go_sdk', previous)
        old_sdk = importlib.util.module_from_spec(old_spec)
        old_spec.loader.exec_module(old_sdk)
        old_sdk.prepare(self.prefix, self.prefix, self.info, HERE / 'go-dqlite-unix-socket.patch')
        self.assertEqual(sdk.verify(self.prefix / 'go-sdk')[1], 'overlay')
        old_flag = '-overlay=' + str(self.prefix / 'go-sdk/go-compiler-overlay.json')
        result = self.command('prepare', dict(self.env, GOFLAGS='-trimpath ' + old_flag))
        self.assertEqual(result.returncode, 0, result.stderr)
        manifest, mode = sdk.verify(self.prefix / 'go-sdk')
        self.assertEqual(mode, 'modfile')
        self.assertTrue(manifest['upgraded_from_overlay'])
        new_flag = '-modfile=' + str(self.prefix / 'go-sdk/gpuworks-native.mod')
        result = subprocess.run(['bash', '-c', 'source "$1" && source "$1" && printf "%s\\n" "$GOFLAGS"',
                                 'fixture', str(self.prefix / 'go-sdk/env.sh')],
                                env=dict(self.env, GOFLAGS='-trimpath ' + old_flag), text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), '-trimpath ' + new_flag)
        for other in (old_flag + ' -overlay=/tmp/unknown.json', '-modfile=/tmp/unknown.mod'):
            with self.subTest(other=other):
                result = self.command('flags', dict(self.env, GOFLAGS=other))
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('unknown GOFLAGS', result.stderr)

    def test_unknown_old_recipe_is_not_upgraded_even_with_rehashed_manifest(self):
        previous = self.root / 'previous-helper.py'
        previous.write_bytes(OLD_HELPER)
        old_spec = importlib.util.spec_from_file_location('previous_go_sdk', previous)
        old_sdk = importlib.util.module_from_spec(old_spec)
        old_spec.loader.exec_module(old_sdk)
        for name in ('prepare_go_sdk.py', 'env.sh'):
            with self.subTest(artifact=name):
                if (self.prefix / 'go-sdk').exists():
                    shutil.rmtree(self.prefix / 'go-sdk')
                old_sdk.prepare(self.prefix, self.prefix, self.info, HERE / 'go-dqlite-unix-socket.patch')
                path = self.prefix / 'go-sdk' / name
                path.write_bytes(path.read_bytes() + b'\n# unknown recipe\n')
                manifest_path = self.prefix / 'go-sdk/manifest.json'
                manifest = json.loads(manifest_path.read_text())
                manifest['files'][name] = sdk.digest(path)
                manifest_path.write_text(json.dumps(manifest))
                result = self.command('prepare')
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('unknown old Go SDK', result.stderr)
                self.assertTrue((self.prefix / 'go-sdk/go-compiler-overlay.json').is_file())

    def test_staged_output_maps_final_prefix(self):
        published = self.root / 'published'
        self.prepare(target=published)
        shutil.move(self.prefix, published)
        result = subprocess.run(['python3', str(published / 'go-sdk/prepare_go_sdk.py'), 'flags',
                                 '--prefix', str(published)], env=self.env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), '-modfile=' + str(published / 'go-sdk/gpuworks-native.mod'))

    def test_main_staged_build_accepts_only_verified_target_own_modfile(self):
        self.prepare()
        old_manifest = (self.prefix / 'go-sdk/manifest.json').read_bytes()
        staged = self.root / 'staged-native'
        staged.mkdir()
        (staged / '.gpuworks-dqlite').write_text('verified-C-fingerprint\n')
        own = '-modfile=' + str(self.prefix / 'go-sdk/gpuworks-native.mod')
        args = ['python3', str(HERE / 'prepare_go_sdk.py'), 'prepare', '--prefix', str(staged),
                '--target-prefix', str(self.prefix), '--repo', str(self.repo)]
        result = subprocess.run(args, env=dict(self.env, GOFLAGS='-trimpath ' + own),
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        manifest, mode = sdk.verify(staged / 'go-sdk', self.prefix / 'go-sdk')
        self.assertEqual(mode, 'modfile')
        self.assertEqual(manifest['modfile'], str(self.prefix / 'go-sdk/gpuworks-native.mod'))
        self.assertEqual((self.prefix / 'go-sdk/manifest.json').read_bytes(), old_manifest)
        result = subprocess.run(args, env=dict(self.env, GOFLAGS='-trimpath ' + own),
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(sdk.verify(staged / 'go-sdk', self.prefix / 'go-sdk')[0], manifest)
        result = subprocess.run(args, env=dict(self.env, GOFLAGS=own + ' -modfile=/tmp/foreign.mod'),
                                text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('unknown GOFLAGS', result.stderr)
        (self.prefix / 'go-sdk/module/app/app.go').write_bytes(b'unknown target override')
        result = subprocess.run(args, env=dict(self.env, GOFLAGS=own), text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('artifact hash mismatch', result.stderr)

    def test_unknown_version_replacement_incomplete_source_and_symlink_are_rejected(self):
        for changes in ({'Version': 'v3.0.5'}, {'Replace': {'Dir': '/untrusted'}}, {'Path': 'another/module'}, {'Dir': ''}):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                sdk.prepare(self.prefix, self.prefix, dict(self.info, **changes),
                            HERE / 'go-dqlite-unix-socket.patch', self.repo, str(self.real_go))
        path = self.module / 'app/options.go'
        original = path.read_bytes()
        path.write_bytes(original + b'\n')
        with self.assertRaisesRegex(ValueError, 'source hash'):
            self.prepare()
        path.unlink()
        path.symlink_to(self.original / 'app/options.go')
        with self.assertRaisesRegex(ValueError, 'non-regular'):
            self.prepare()
        self.assertFalse((self.prefix / 'go-sdk').exists())

    def test_changed_patch_or_generated_artifact_or_root_module_is_rejected(self):
        changed = self.root / 'go-dqlite-unix-socket.patch'
        changed.write_bytes((HERE / changed.name).read_bytes() + b'\n')
        with self.assertRaisesRegex(ValueError, 'patch hash'):
            sdk.prepare(self.prefix, self.prefix, self.info, changed, self.repo, str(self.real_go))
        self.prepare()
        (self.repo / 'go.mod').write_text('module changed/root\n')
        result = self.command('flags')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('repository module files differ', result.stderr)
        (self.prefix / 'go-sdk/module/app/app.go').write_bytes(b'untracked diagnostic patch')
        with self.assertRaisesRegex(ValueError, 'artifact hash'):
            self.prepare()

    def test_flags_preserve_unrelated_and_reject_foreign_overrides(self):
        flags = '-race -trimpath -tags=dqlite,nosqlite3'
        expected = flags + ' -modfile=' + str(self.prefix / 'go-sdk/gpuworks-native.mod')
        self.assertEqual(sdk.flag_mode(flags, self.prefix, 'modfile'), expected)
        self.assertEqual(sdk.flag_mode(expected, self.prefix, 'modfile'), expected)
        self.assertEqual(sdk.flag_mode(expected, self.prefix, 'modfile', prepare=True), flags)
        for other in ('-overlay=/tmp/diagnostic.json', '--overlay=/tmp/diagnostic.json',
                      '-modfile=/tmp/foreign.mod', '-modfile /tmp/foreign.mod',
                      expected + ' -modfile=/tmp/foreign.mod'):
            with self.subTest(flags=other), self.assertRaisesRegex(ValueError, 'unknown GOFLAGS'):
                sdk.flag_mode(other, self.prefix, 'modfile')

    def test_sdk_only_native_env_keeps_C_artifacts_and_Go_flags(self):
        library = self.prefix / 'lib.a'
        library.write_bytes(b'verified cached C library')
        (self.prefix / 'artifacts.sha256').write_text(sdk.digest(library) + '  lib.a\n')
        original_env = 'export CC=fixture-musl\nexport CGO_CFLAGS=-Ifixture\nexport CGO_LDFLAGS=-Lfixture\n'
        (self.prefix / 'env.sh').write_text(original_env)
        flags = '-race -trimpath -tags=dqlite,nosqlite3'
        build_env = dict(self.env, GOFLAGS=flags)
        result = subprocess.run(['bash', str(HERE.parent / 'build_dqlite.sh'), '--go-sdk-only',
                                 '--output', str(self.prefix)], env=build_env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(library.read_bytes(), b'verified cached C library')
        self.assertTrue((self.prefix / 'env.sh').read_text().startswith(original_env))
        shell = 'source "$1" && source "$1" && printf "%s\\n" "$GOFLAGS" "$CC" "$CGO_CFLAGS" "$CGO_LDFLAGS"'
        result = subprocess.run(['bash', '-c', shell, 'fixture', str(self.prefix / 'env.sh')],
                                env=build_env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.splitlines(), [flags + ' -modfile=' + str(self.prefix / 'go-sdk/gpuworks-native.mod'),
                                                     'fixture-musl', '-Ifixture', '-Lfixture'])
        for conflict in ({'GOFLAGS': '-overlay=/tmp/diagnostic.json'},
                         {'GOFLAGS': '', 'FIXTURE_PERSISTED_GOFLAGS': '-modfile=/tmp/persisted.mod'}):
            with self.subTest(conflict=conflict):
                result = subprocess.run(['bash', '-c', 'source "$1"', 'fixture', str(self.prefix / 'env.sh')],
                                        env=dict(build_env, **conflict), text=True, capture_output=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('unknown GOFLAGS', result.stderr)

    def test_sdk_only_refuses_unverified_native_prefix(self):
        (self.prefix / 'env.sh').write_text('export CC=fixture\n')
        (self.prefix / 'artifacts.sha256').write_text('0' * 64 + '  missing.a\n')
        result = subprocess.run(['bash', str(HERE.parent / 'build_dqlite.sh'), '--go-sdk-only',
                                 '--output', str(self.prefix)], env=self.env, text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.prefix / 'go-sdk').exists())

    def test_missing_sdk_and_toolchain_never_contact_inherited_network_proxy(self):
        requests = []

        class ProxyTrap(BaseHTTPRequestHandler):
            def do_GET(self):
                requests.append(self.path)
                self.send_error(404)

            def log_message(self, *args):
                pass

        with ThreadingHTTPServer(('127.0.0.1', 0), ProxyTrap) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                inherited = dict(self.env, GO=str(self.real_go), GOTOOLCHAIN='auto',
                                 GOPROXY='http://127.0.0.1:' + str(server.server_port),
                                 GOSUMDB='sum.golang.org', GOPRIVATE='*', GONOPROXY='*', GOVCS='*:all',
                                 GOMODCACHE=str(self.root / 'empty-module-cache'))
                result = self.command('prepare', inherited)
                self.assertNotEqual(result.returncode, 0)
                self.assertTrue('GOPROXY=off' in result.stderr or
                                'must already be present in the module cache' in result.stderr, result.stderr)
                self.assertEqual(requests, [])
                self.assertFalse((self.prefix / 'go-sdk').exists())
                (self.repo / 'go.mod').write_text('module fixture/root\n\ngo 1.99.0\n\nrequire '
                                                + sdk.MODULE + ' ' + sdk.VERSION + '\n')
                result = self.command('prepare', inherited)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('GOTOOLCHAIN=local', result.stderr)
                self.assertEqual(requests, [])
                self.assertFalse((self.root / 'empty-module-cache/golang.org').exists())
                self.assertFalse((self.prefix / 'go-sdk').exists())
            finally:
                server.shutdown()
                thread.join(timeout=5)
                self.assertFalse(thread.is_alive())

    def test_real_go_126_modfile_compiles_under_cache_source_where_overlay_fails(self):
        cache = self.root / 'isolated-module-cache'
        dependency = cache / 'example.test/socket@v1.0.0'
        dependency.mkdir(parents=True)
        (dependency / 'go.mod').write_text('module example.test/socket\n\ngo 1.26.7\n')
        (dependency / 'socket.go').write_text('package socket\nfunc WithUnixSocket(name string) string { return "ignored" }\n')
        root = self.root / 'compile-fixture'
        root.mkdir()
        (root / 'go.mod').write_text('module fixture/compile\n\ngo 1.26.7\n\nrequire example.test/socket v1.0.0\n\nreplace example.test/socket => ' + str(dependency) + '\n')
        (root / 'main.go').write_text('package main\nimport "example.test/socket"\nfunc main() { if socket.WithUnixSocket("@private") != "@private" { panic("socket ignored") } }\n')
        patched = self.root / 'private-module'
        shutil.copytree(dependency, patched)
        (patched / 'socket.go').write_text('package socket\nfunc WithUnixSocket(name string) string { return name }\n')
        overlay = root / 'old-overlay.json'
        overlay.write_text(json.dumps({'Replace': {str(dependency / 'socket.go'): str(patched / 'socket.go')}}))
        env = dict(self.env, GOMODCACHE=str(cache), GOCACHE=os.environ.get('GOCACHE', str(self.root / 'go-build-cache')), CGO_ENABLED='0', TMPDIR=os.environ.get('GOTMPDIR', tempfile.gettempdir()))
        old = subprocess.run([str(self.real_go), 'build', '-p=1', '-overlay=' + str(overlay), '.'], cwd=root,
                             env=env, capture_output=True, text=True, timeout=60)
        self.assertNotEqual(old.returncode, 0)
        self.assertIn('Files beneath GOMODCACHE', old.stderr)
        mod = root / 'private.mod'
        shutil.copyfile(root / 'go.mod', mod)
        subprocess.run([str(self.real_go), 'mod', 'edit', '-modfile=' + str(mod),
                        '-replace=example.test/socket=' + str(patched)], cwd=root, env=env, check=True)
        good = subprocess.run([str(self.real_go), 'build', '-p=1', '-modfile=' + str(mod), '-o', str(root / 'compiled'), '.'],
                              cwd=root, env=env, capture_output=True, text=True, timeout=60)
        self.assertEqual(good.returncode, 0, good.stderr)
        subprocess.run([str(root / 'compiled')], env=env, check=True, timeout=10)
        self.assertEqual((dependency / 'socket.go').read_text(), 'package socket\nfunc WithUnixSocket(name string) string { return "ignored" }\n')
        self.assertNotIn(str(patched), (root / 'go.mod').read_text())


if __name__ == '__main__':
    unittest.main()
